import json
import logging
from collections.abc import Sequence
from dataclasses import dataclass
from datetime import datetime, timezone
from threading import Event, RLock, Thread
from typing import Any

from .agent import AgentStopped, run_messages
from .config import build_openai_client, get_runtime_config
from .run_store import RunStore
from .vision import prepare_user_item


logger = logging.getLogger(__name__)


@dataclass(frozen=True)
class RunJob:
    cancel_event: Event
    thread: Thread
    started_at: str


class RunJobConflictError(RuntimeError):
    """同一运行 ID 已有活动任务。"""


class RunJobManager:
    """在后台线程中运行 Agent，并提供协作式停止和继续。"""

    def __init__(self) -> None:
        self._jobs: dict[str, RunJob] = {}
        self._lock = RLock()

    def submit_message(
        self,
        run_id: str,
        content: str,
        attachments: Sequence[str] = (),
        instance_context: Any = None,
    ) -> RunJob:
        if not content.strip():
            raise ValueError("消息内容不能为空。")
        return self._start_job(
            run_id,
            display_text=content.strip(),
            attachments=attachments,
            instance_context=instance_context,
        )

    def continue_run(self, run_id: str) -> RunJob:
        return self._start_job(run_id)

    def stop_run(self, run_id: str) -> bool:
        with self._lock:
            job = self._jobs.get(run_id)
            if job is None or not job.thread.is_alive():
                return False
            job.cancel_event.set()
        RunStore(run_id).set_status("stopping", "用户从 WebUI 请求停止")
        return True

    def is_running(self, run_id: str) -> bool:
        with self._lock:
            job = self._jobs.get(run_id)
            return bool(job is not None and job.thread.is_alive())

    def _start_job(
        self,
        run_id: str,
        display_text: str | None = None,
        attachments: Sequence[str] = (),
        instance_context: Any = None,
    ) -> RunJob:
        with self._lock:
            existing = self._jobs.get(run_id)
            if existing is not None and existing.thread.is_alive():
                raise RunJobConflictError("当前对话仍在执行。")

            store = RunStore(run_id)
            input_items = store.load_input_items()
            if display_text is None and not input_items:
                raise ValueError("当前对话没有可继续的上下文。")
            store.start()
            if display_text is not None:
                model_text = _build_model_text(
                    display_text,
                    attachments,
                    instance_context,
                    run_id,
                )
                user_item = {"role": "user", "content": model_text}
                input_items.append(user_item)
                store.append_event(
                    "user_message",
                    {
                        "item": user_item,
                        "display_text": display_text,
                        "attachments": list(attachments),
                        "instance_context": instance_context,
                    },
                )
                store.save_context(input_items)

            cancel_event = Event()
            thread = Thread(
                target=self._run_job,
                args=(run_id, input_items, store, cancel_event, tuple(attachments)),
                name=f"webweave-run-{run_id}",
                daemon=True,
            )
            job = RunJob(
                cancel_event=cancel_event,
                thread=thread,
                started_at=datetime.now(timezone.utc).isoformat(),
            )
            self._jobs[run_id] = job
            thread.start()
            return job

    @staticmethod
    def _run_job(
        run_id: str,
        input_items: list[Any],
        store: RunStore,
        cancel_event: Event,
        attachments: Sequence[str],
    ) -> None:
        try:
            if attachments and input_items:
                input_items[-1] = prepare_user_item(
                    str(input_items[-1].get("content", "")),
                    attachments,
                    run_id,
                    get_runtime_config().llm.vision,
                )
                store.save_context(input_items)
            client = build_openai_client()
            run_messages(
                client,
                input_items,
                verbose=False,
                run_id=run_id,
                run_store=store,
                should_stop=cancel_event.is_set,
            )
        except AgentStopped:
            return
        except Exception as exc:
            logger.exception("WebUI Agent 后台任务失败: %s", run_id)
            if store.read_state().get("status") != "failed":
                store.set_status("failed", str(exc))


def _build_model_text(
    content: str,
    attachments: Sequence[str],
    instance_context: Any,
    run_id: str,
) -> str:
    sections = [content]
    if attachments:
        paths = "\n".join(
            f"- data/resources/{run_id}/{name}" for name in attachments
        )
        sections.append(f"[本次附件]\n{paths}")
    if instance_context:
        sections.append(
            "[实例检查上下文]\n"
            + json.dumps(instance_context, ensure_ascii=False, indent=2)
        )
    return "\n\n".join(sections)


RUN_JOB_MANAGER = RunJobManager()
