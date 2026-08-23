import json
import os
import re
from collections.abc import Mapping, Sequence
from datetime import datetime, timezone
from pathlib import Path
from threading import RLock
from typing import Any

from .config import RUN_ROOT


_RUN_LOCKS: dict[str, RLock] = {}
_RUN_LOCKS_GUARD = RLock()


def _timestamp() -> str:
    return datetime.now(timezone.utc).isoformat()


def to_jsonable(value: Any) -> Any:
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if hasattr(value, "model_dump"):
        try:
            return to_jsonable(value.model_dump(mode="json"))
        except TypeError:
            return to_jsonable(value.model_dump())
    if hasattr(value, "to_dict"):
        return to_jsonable(value.to_dict())
    if isinstance(value, Mapping):
        return {str(key): to_jsonable(item) for key, item in value.items()}
    if isinstance(value, Sequence) and not isinstance(value, (str, bytes, bytearray)):
        return [to_jsonable(item) for item in value]
    return str(value)


class RunStore:
    """保存单次运行的原始事件、状态和可恢复上下文。"""

    def __init__(self, run_id: str) -> None:
        if not re.fullmatch(r"[A-Za-z0-9_-]{1,64}", run_id):
            raise ValueError("run_id 无效。")
        self.run_id = run_id
        self.run_dir = (RUN_ROOT / run_id).resolve()
        if RUN_ROOT.resolve() not in self.run_dir.parents:
            raise ValueError("run_id 越权。")
        self.events_path = self.run_dir / "events.jsonl"
        self.state_path = self.run_dir / "state.json"
        self.context_path = self.run_dir / "context.json"
        with _RUN_LOCKS_GUARD:
            self._lock = _RUN_LOCKS.setdefault(run_id, RLock())
        self._started = False
        self.run_dir.mkdir(parents=True, exist_ok=True)

    def start(self) -> None:
        with self._lock:
            if self._started:
                return
            previous_state = self.read_state()
            resumed = bool(
                previous_state
                or self.context_path.exists()
                or self.events_path.exists()
            )
            self.append_event(
                "run_resumed" if resumed else "run_started",
                {"previous_status": previous_state.get("status")},
                status="running",
            )
            self._started = True

    def append_event(
        self,
        event_type: str,
        payload: Mapping[str, Any] | None = None,
        status: str | None = None,
    ) -> None:
        with self._lock:
            event = {
                "timestamp": _timestamp(),
                "type": event_type,
                "payload": to_jsonable(payload or {}),
            }
            with self.events_path.open("a", encoding="utf-8") as stream:
                stream.write(json.dumps(event, ensure_ascii=False) + "\n")
                stream.flush()
                os.fsync(stream.fileno())

            state = self.read_state()
            state.update(
                {
                    "run_id": self.run_id,
                    "status": status or state.get("status", "running"),
                    "updated_at": event["timestamp"],
                    "last_event": event_type,
                    "event_count": int(state.get("event_count", 0)) + 1,
                }
            )
            self._write_json(self.state_path, state)

    def set_status(self, status: str, reason: str = "") -> None:
        self.append_event(
            "run_status",
            {"status": status, "reason": reason},
            status=status,
        )

    def save_instance(self, instance: Mapping[str, Any]) -> None:
        """保存实例状态，并追加统一的实例状态事件。"""
        with self._lock:
            self.append_event("instance_status", {"instance": instance})
            state = self.read_state()
            state["instance"] = to_jsonable(instance)
            self._write_json(self.state_path, state)

    def save_context(self, input_items: Sequence[Any], summary: Any = None) -> None:
        with self._lock:
            context = {
                "version": 1,
                "updated_at": _timestamp(),
                "summary": to_jsonable(summary),
                "input_items": to_jsonable(list(input_items)),
            }
            self._write_json(self.context_path, context)

            state = self.read_state()
            state.update(
                {
                    "run_id": self.run_id,
                    "context_updated_at": context["updated_at"],
                    "context_version": int(state.get("context_version", 0)) + 1,
                }
            )
            self._write_json(self.state_path, state)

    def load_input_items(self) -> list[Any]:
        context = self._read_json(self.context_path)
        items = context.get("input_items")
        if isinstance(items, list):
            return items
        return self._rebuild_input_items()

    def read_state(self) -> dict[str, Any]:
        state = self._read_json(self.state_path)
        return state if isinstance(state, dict) else {}

    def read_context(self) -> dict[str, Any]:
        context = self._read_json(self.context_path)
        return context if isinstance(context, dict) else {}

    def _rebuild_input_items(self) -> list[Any]:
        items: list[Any] = []
        if not self.events_path.exists():
            return items
        with self.events_path.open("r", encoding="utf-8") as stream:
            for line in stream:
                try:
                    event = json.loads(line)
                except json.JSONDecodeError:
                    continue
                if not isinstance(event, Mapping):
                    continue
                payload = event.get("payload", {})
                if not isinstance(payload, Mapping):
                    continue
                event_type = event.get("type")
                if event_type == "user_message" or event_type == "tool_output":
                    item = payload.get("item")
                    if isinstance(item, Mapping):
                        items.append(item)
                elif event_type == "model_output":
                    output_items = payload.get("items", [])
                    if isinstance(output_items, list):
                        items.extend(
                            item for item in output_items if isinstance(item, Mapping)
                        )
                    if not output_items:
                        output_text = payload.get("output_text")
                        if isinstance(output_text, str) and output_text:
                            items.append(
                                {"role": "assistant", "content": output_text}
                            )
                elif event_type == "context_compressed":
                    compressed_items = payload.get("input_items", [])
                    if isinstance(compressed_items, list):
                        items = [
                            item
                            for item in compressed_items
                            if isinstance(item, Mapping)
                        ]
        return items

    @staticmethod
    def _read_json(path: Path) -> dict[str, Any]:
        if not path.exists():
            return {}
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            return {}
        return payload if isinstance(payload, dict) else {}

    @staticmethod
    def _write_json(path: Path, payload: Mapping[str, Any]) -> None:
        temporary_path = path.with_name(f".{path.name}.tmp")
        temporary_path.write_text(
            json.dumps(payload, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        os.replace(temporary_path, path)
