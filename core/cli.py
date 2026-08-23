from uuid import uuid4

from .agent import run_messages
from .config import build_openai_client
from .paths import prepare_run_directories
from .run_store import RunStore


def chat(run_id: str | None = None) -> None:
    print("Frontend Agent 已启动。输入 q 退出。")
    session_run_id = run_id or uuid4().hex
    store = RunStore(session_run_id)
    store.start()
    session_directories = prepare_run_directories(session_run_id)
    print(f"当前运行 ID: {session_run_id}")
    if run_id:
        print(f"已加载运行记录: {store.run_dir}")
    print(
        "当前运行目录: "
        f"{session_directories['resources_dir']}, "
        f"{session_directories['result_dir']}, "
        f"{session_directories['download_dir']}"
    )

    input_items = store.load_input_items()
    try:
        client = build_openai_client()
    except RuntimeError as exc:
        print(f"Agent 配置错误: {exc}")
        return

    while True:
        try:
            user_text = input("\n你: ").strip()
        except KeyboardInterrupt:
            store.set_status("stopped", "用户中断")
            print("\n已中断。")
            break

        if user_text.lower() in {"q", "quit", "exit"}:
            store.set_status("stopped", "用户退出")
            print("已退出。")
            break
        if not user_text:
            print("请输入有效问题。")
            continue

        user_item = {"role": "user", "content": user_text}
        input_items.append(user_item)
        store.append_event("user_message", {"item": user_item})
        store.save_context(input_items)
        try:
            run_messages(
                client,
                input_items,
                verbose=True,
                run_id=session_run_id,
                run_store=store,
            )
        except KeyboardInterrupt:
            print("\n已中断。")
            break
        except Exception as exc:
            print(f"\nAgent 运行异常: {exc}")
