"""Offline fidelity smoke evaluation; never contacts the task model API."""

from __future__ import annotations

import argparse
import json
import sys
import time
from contextlib import redirect_stdout
from dataclasses import dataclass
from pathlib import Path
from typing import TypedDict

from worker import CompressionRequest, compress


@dataclass(frozen=True)
class Case:
    name: str
    text: str
    exact: tuple[str, ...]


class CaseReport(TypedDict):
    name: str
    input_tokens: int
    output_tokens: int
    retained_ratio: float
    seconds: float
    missing_exact_facts: list[str]


class EvaluationReport(TypedDict):
    model: str
    device: str
    rate: float
    tokenizer: str
    peak_cuda_bytes: int
    cases: list[CaseReport]
    task_completion_evaluated: bool


def cases() -> list[Case]:
    chinese = "校园网站采用简洁明快的视觉方向，页面以清晰的层次组织介绍内容，背景保持克制，正文突出学校文化与活动氛围。\n"
    english = (
        "The page uses a restrained visual hierarchy and generous spacing. "
        "Background descriptions explain the setting and repeat decorative "
        "context without introducing additional requirements.\n"
    )
    return [
        Case(
            "中文网页资料",
            chinese * 30 + "必须使用中文，仅生成单页。\n"
            "桌面宽度 1280，移动端断点 768。\n"
            "源码保存在 index.html、style.css、script.js。\n" + chinese * 30,
            ("必须使用中文，仅生成单页。", "1280", "768", "index.html", "style.css", "script.js"),
        ),
        Case(
            "英文资料",
            english * 30 + "Never remove the contact form.\n"
            "The form posts to /api/contact with timeout 30 seconds.\n" + english * 30,
            ("Never remove the contact form.", "/api/contact", "30 seconds"),
        ),
    ]


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--model", default="microsoft/llmlingua-2-xlm-roberta-large-meetingbank"
    )
    parser.add_argument("--device", choices=("cpu", "cuda"), default="cuda")
    parser.add_argument("--rate", type=float, default=0.7)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    with redirect_stdout(sys.stderr):
        import torch
        from llmlingua import PromptCompressor

        model = PromptCompressor(
            model_name=args.model, use_llmlingua2=True, device_map=args.device
        )
    reports: list[CaseReport] = []
    for case in cases():
        started = time.monotonic()
        result = compress(CompressionRequest(case.text, args.rate), model)
        reports.append({
            "name": case.name,
            "input_tokens": result.input_tokens,
            "output_tokens": result.output_tokens,
            "retained_ratio": result.output_tokens / result.input_tokens,
            "seconds": round(time.monotonic() - started, 3),
            "missing_exact_facts": [
                value for value in case.exact if value not in result.text
            ],
        })
    report: EvaluationReport = {
        "model": args.model, "device": args.device, "rate": args.rate,
        "tokenizer": "cl100k_base (proxy, not provider billing)",
        "peak_cuda_bytes": (
            torch.cuda.max_memory_allocated() if args.device == "cuda" else 0
        ),
        "cases": reports, "task_completion_evaluated": False,
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(
        json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
    )
    print(json.dumps(report, ensure_ascii=False))
    if any(case["missing_exact_facts"] for case in reports):
        raise SystemExit("Exact facts were lost")


if __name__ == "__main__":
    main()
