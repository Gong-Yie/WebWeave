"""Persistent LLMLingua-2 worker; stdout is reserved for the JSONL protocol."""

from __future__ import annotations

import argparse
import json
import logging
import math
import re
import sys
from contextlib import redirect_stdout
from dataclasses import asdict, dataclass
from typing import Any, Protocol, TextIO

logger = logging.getLogger(__name__)
PROTECTED_LINE = re.compile(
    r"\d|https?://|[\w./\\-]+\.(?:html|css|js|ts|py|go|json)\b"
    r"|禁止|不得|必须|不能|不可|不要|仅|\b(?:not|never|must)\b",
    re.IGNORECASE,
)


class Compressor(Protocol):
    def compress_prompt(self, context: str, **kwargs: Any) -> dict[str, Any]: ...


@dataclass(frozen=True)
class CompressionRequest:
    text: str
    rate: float

    @classmethod
    def parse(cls, line: str) -> CompressionRequest:
        value = json.loads(line)
        if not isinstance(value, dict) or set(value) != {"text", "rate"}:
            raise ValueError("request must contain text and rate")
        text, rate = value["text"], value["rate"]
        if not isinstance(text, str) or not text:
            raise ValueError("text must be a non-empty string")
        if isinstance(rate, bool) or not isinstance(rate, (int, float)):
            raise ValueError("rate must be a number")
        if not math.isfinite(rate) or not 0 < rate <= 1:
            raise ValueError("rate must be in (0, 1]")
        return cls(text, float(rate))


@dataclass(frozen=True)
class CompressionResponse:
    ok: bool
    text: str = ""
    input_tokens: int = 0
    output_tokens: int = 0
    error: str = ""


def count_tokens(text: str) -> int:
    import tiktoken

    return len(tiktoken.get_encoding("cl100k_base").encode(text))


def compress(request: CompressionRequest, model: Compressor) -> CompressionResponse:
    parts: list[str] = []
    pending: list[str] = []
    fence: str | None = None

    def flush() -> None:
        if not pending:
            return
        original = "".join(pending)
        result = model.compress_prompt(
            original, rate=request.rate,
            force_tokens=["\n"], force_reserve_digit=True, drop_consecutive=False,
        )
        text = result.get("compressed_prompt")
        if not isinstance(text, str) or not text.strip():
            raise ValueError("compressor returned empty text")
        if original.endswith("\n") and not text.endswith("\n"):
            text += "\n"
        parts.append(text)
        pending.clear()

    for line in request.text.splitlines(keepends=True):
        stripped = line.lstrip()
        marker = ""
        if stripped.startswith(("```", "~~~")):
            marker = stripped[0] * (len(stripped) - len(stripped.lstrip(stripped[0])))
        if fence is not None or marker or PROTECTED_LINE.search(line):
            flush()
            parts.append(line)
            if fence is None and marker:
                fence = marker
            elif fence is not None and marker.startswith(fence):
                fence = None
        else:
            pending.append(line)
    flush()
    text = "".join(parts)
    before, after = count_tokens(request.text), count_tokens(text)
    # Protecting facts can legitimately leave nothing safe to shorten.
    if after >= before:
        text, after = request.text, before
    return CompressionResponse(True, text, before, after)


def serve(source: TextIO, target: TextIO, model: Compressor) -> None:
    for line in source:
        try:
            request = CompressionRequest.parse(line)
            # Third-party diagnostics must never corrupt the protocol stream.
            with redirect_stdout(sys.stderr):
                response = compress(request, model)
        except Exception as exc:
            logger.exception("Compression request failed")
            response = CompressionResponse(False, error=f"{type(exc).__name__}: {exc}")
        target.write(json.dumps(asdict(response), ensure_ascii=False) + "\n")
        target.flush()


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--model", required=True)
    parser.add_argument("--device", choices=("cpu", "cuda"), required=True)
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, stream=sys.stderr)
    with redirect_stdout(sys.stderr):
        from llmlingua import PromptCompressor

        model = PromptCompressor(
            model_name=args.model, use_llmlingua2=True, device_map=args.device
        )
    logger.info("Compression model loaded on %s", args.device)
    serve(sys.stdin, sys.stdout, model)


if __name__ == "__main__":
    if hasattr(sys.stdin, "reconfigure"):
        sys.stdin.reconfigure(encoding="utf-8")
        sys.stdout.reconfigure(encoding="utf-8")
    main()
