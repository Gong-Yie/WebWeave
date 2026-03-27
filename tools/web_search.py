import os
import warnings
from dotenv import load_dotenv
from langchain.tools import tool

try:
    # 新版包（如已安装）
    from langchain_tavily import TavilySearch  # type: ignore
    USING_NEW_TAVILY = True
except Exception:
    # 兼容：旧版包（避免当前环境缺依赖导致启动失败）
    from langchain_community.tools.tavily_search import TavilySearchResults as TavilySearch  # type: ignore
    USING_NEW_TAVILY = False
    warnings.filterwarnings(
        "ignore",
        message=r"The class `TavilySearchResults` was deprecated.*",
    )

load_dotenv()

tavily_api_key = os.getenv("TAVILY_API_KEY")

if tavily_api_key:
    kwargs = {
        "max_results": 5,
        "description": """
        专门用于编程问题联网搜索的工具。当需要样例代码、查询某些函数的功能、或者需要最新的 API 文档时调用此工具。
        """,
    }
    if USING_NEW_TAVILY:
        kwargs["tavily_api_key"] = tavily_api_key
    else:
        # 旧版通常直接从环境变量读取 key
        os.environ["TAVILY_API_KEY"] = tavily_api_key
    tavily_tool = TavilySearch(**kwargs)
else:
    @tool("tavily_tool")
    def tavily_tool(query: str) -> str:
        return "未设置环境变量 TAVILY_API_KEY，无法进行联网搜索。请补充 TAVILY_API_KEY 后重试。"