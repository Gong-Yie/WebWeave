import os
import json
from pathlib import Path
from dotenv import load_dotenv
from langchain_openai import ChatOpenAI
from langchain_core.messages import HumanMessage, SystemMessage, ToolMessage

from tools.web_search import tavily_tool
from tools.resource_pipeline import (
    scan_resources_source,
    analyze_resources_stack,
    propose_build_questions,
    generate_project_structure,
    generate_file_content,
    generate_readme,
    build_web_project_from_resources,
)

load_dotenv()

WORKSPACE_ROOT = Path(__file__).resolve().parent
ALLOWED_DIRS = {
    "resources_dir": WORKSPACE_ROOT / "resources",
    "result_dir": WORKSPACE_ROOT / "result",
}

tools = [
    tavily_tool,
    scan_resources_source,
    analyze_resources_stack,
    propose_build_questions,
    generate_project_structure,
    generate_file_content,
    generate_readme,
    build_web_project_from_resources,
]

llm = ChatOpenAI(
    model="deepseek-chat",
    temperature=0.7,
    api_key=os.getenv("DEEPSEEK_API_KEY"),
    base_url="https://api.deepseek.com/v1",
)

SYSTEM_PROMPT = """
你是一个"网页项目生成型 Agent"。
你的目标是：读取 resources 里的源码/素材，通过大模型识别技术栈与前后端属性，主动提出澄清问题，然后生成一个完整可运行的网页项目到 result，并写好 README.md。

可用工具能力：
1) scan_resources_source: 扫描 resources 源码概要（文件清单+关键片段）
2) analyze_resources_stack: 让大模型识别技术栈，并判断是前端/后端/全栈
3) propose_build_questions: 让大模型基于识别结果提出需要向用户澄清的问题
4) generate_project_structure: 根据用户需求和技术栈，生成完整的项目结构
5) generate_file_content: 根据项目结构和需求，生成指定文件的内容
6) generate_readme: 生成项目的 README.md 文件内容
7) build_web_project_from_resources: 根据需求、技术栈和项目结构，生成完整的网页项目并保存到 result 目录
8) tavily_tool: 联网检索技术资料（当用户提到模板/主题/最新文档时使用）

工作原则：
- 能用工具完成就先用工具，不要只给口头建议。
- 默认流程：
  1. 首先使用 scan_resources_source 扫描 resources 目录
  2. 然后使用 analyze_resources_stack 分析技术栈和前后端属性
  3. 接着使用 propose_build_questions 提出澄清问题
  4. 等待用户回答后，使用 generate_project_structure 生成项目结构
  5. 然后使用 build_web_project_from_resources 生成完整项目（内部会逐个生成文件）
- 在用户回答完澄清问题之前，不要开始生成项目。
- 若用户提到 Hexo 主题/模板名称，优先调用 tavily_tool 搜索对应文档/仓库要点再生成。
- 安全限制：你只能读写 resources/result 两个目录，不允许访问其他目录。
- 若目录为空或信息不足，要明确说明缺失项并给下一步操作。
- 所有生成的网站内容都必须使用中文。
- 结果要简洁、可执行。
""".strip()


def _build_tool_map():
    return {tool.name: tool for tool in tools}


def _resolve_path(raw: str) -> Path:
    p = Path(raw)
    if not p.is_absolute():
        p = WORKSPACE_ROOT / p
    return p.resolve()


def _is_path_allowed(raw: str) -> bool:
    target = _resolve_path(raw)
    for root in ALLOWED_DIRS.values():
        if target == root or root in target.parents:
            return True
    return False


def _sanitize_tool_args(tool_name: str, tool_args: dict) -> dict:
    args = dict(tool_args or {})

    # 强制默认目录参数只落在白名单目录中
    for key, default_root in ALLOWED_DIRS.items():
        if key not in args or not str(args[key]).strip():
            args[key] = str(default_root)

    # 对 *_dir 参数做白名单校验
    for key, value in args.items():
        if key.endswith("_dir") and isinstance(value, str):
            if not _is_path_allowed(value):
                raise ValueError(f"参数 {key} 越权: {value}。仅允许 resources/result 目录。")

    # 针对联网搜索工具，直接忽略路径类参数（如果模型误传）
    if tool_name == "tavily_search_results_json":
        for unsafe_key in ["path", "file_path", "resources_dir", "result_dir"]:
            args.pop(unsafe_key, None)

    return args


def _short_text(text: str, limit: int = 260) -> str:
    t = (text or "").replace("\n", " ").strip()
    if len(t) <= limit:
        return t
    return t[:limit] + "..."


def run_agent(user_input: str, max_iterations: int = 8, verbose: bool = True) -> str:
    if not user_input.strip():
        return "请输入有效问题。"

    tool_map = _build_tool_map()
    model = llm.bind_tools(tools)
    messages = [
        SystemMessage(content=SYSTEM_PROMPT),
        HumanMessage(content=user_input),
    ]

    for i in range(max_iterations):
        if verbose:
            print(f"\n[Agent] 第 {i + 1} 轮思考中...")
        ai_msg = model.invoke(messages)
        messages.append(ai_msg)

        tool_calls = getattr(ai_msg, "tool_calls", None) or []
        if not tool_calls:
            if verbose:
                print("[Agent] 无工具调用，准备输出最终答案。")
            return ai_msg.content if isinstance(ai_msg.content, str) else str(ai_msg.content)

        for call in tool_calls:
            tool_name = call.get("name", "")
            tool_args = call.get("args", {}) or {}
            tool_call_id = call.get("id", "")

            tool = tool_map.get(tool_name)
            if tool is None:
                tool_output = f"工具不存在: {tool_name}"
            else:
                try:
                    safe_args = _sanitize_tool_args(tool_name, tool_args)
                    if verbose:
                        print(f"[Tool] 调用 {tool_name}")
                        print(f"[Tool] 参数 {json.dumps(safe_args, ensure_ascii=False)}")
                    tool_output = tool.invoke(safe_args)
                    if verbose:
                        print(f"[Tool] 结果 {_short_text(str(tool_output))}")
                except Exception as exc:
                    tool_output = f"工具执行失败({tool_name}): {exc}"
                    if verbose:
                        print(f"[Tool] 异常 {tool_output}")

            messages.append(
                ToolMessage(
                    content=str(tool_output),
                    tool_call_id=tool_call_id,
                )
            )

    return "已达到最大工具调用轮数，请缩小问题范围后重试。"


def chat() -> None:
    print("Frontend Agent 已启动。输入 q 退出。")
    print(f"允许访问目录: {ALLOWED_DIRS['resources_dir']}, {ALLOWED_DIRS['result_dir']}")
    
    # 初始化持久的对话历史
    tool_map = _build_tool_map()
    model = llm.bind_tools(tools)
    messages = [
        SystemMessage(content=SYSTEM_PROMPT),
    ]
    
    while True:
        user_text = input("\n你: ").strip()
        if user_text.lower() in {"q", "quit", "exit"}:
            print("已退出。")
            break

        try:
            # 添加用户输入到对话历史
            messages.append(HumanMessage(content=user_text))
            
            # 处理对话
            for i in range(8):  # 最大迭代次数
                print(f"\n[Agent] 第 {i + 1} 轮思考中...")
                ai_msg = model.invoke(messages)
                messages.append(ai_msg)

                tool_calls = getattr(ai_msg, "tool_calls", None) or []
                if not tool_calls:
                    print("[Agent] 无工具调用，准备输出最终答案。")
                    answer = ai_msg.content if isinstance(ai_msg.content, str) else str(ai_msg.content)
                    print(f"\nAgent: {answer}")
                    break

                for call in tool_calls:
                    tool_name = call.get("name", "")
                    tool_args = call.get("args", {}) or {}
                    tool_call_id = call.get("id", "")

                    tool = tool_map.get(tool_name)
                    if tool is None:
                        tool_output = f"工具不存在: {tool_name}"
                    else:
                        try:
                            safe_args = _sanitize_tool_args(tool_name, tool_args)
                            print(f"[Tool] 调用 {tool_name}")
                            print(f"[Tool] 参数 {json.dumps(safe_args, ensure_ascii=False)}")
                            tool_output = tool.invoke(safe_args)
                            print(f"[Tool] 结果 {_short_text(str(tool_output))}")
                        except Exception as exc:
                            tool_output = f"工具执行失败({tool_name}): {exc}"
                            print(f"[Tool] 异常 {tool_output}")

                    messages.append(
                        ToolMessage(
                            content=str(tool_output),
                            tool_call_id=tool_call_id,
                        )
                    )
            else:
                print("\nAgent: 已达到最大工具调用轮数，请缩小问题范围后重试。")
                
        except KeyboardInterrupt:
            print("\n已中断。")
            break
        except Exception as exc:
            print(f"\nAgent 运行异常: {exc}")


if __name__ == "__main__":
    chat()
