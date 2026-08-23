import json
from typing import Any

from skills.loader import SkillLoader


SKILL_LIST_TOOL_DEFINITION: dict[str, Any] = {
    "type": "function",
    "name": "list_skills",
    "description": "发现当前 Agent 可用的本地 Skill，只返回名称、描述和路径。",
    "parameters": {
        "type": "object",
        "properties": {},
        "required": [],
        "additionalProperties": False,
    },
    "strict": False,
}

SKILL_READ_TOOL_DEFINITION: dict[str, Any] = {
    "type": "function",
    "name": "read_skill",
    "description": "读取指定本地 Skill 的完整 SKILL.md 指令。必须先使用 list_skills 获得合法名称。",
    "parameters": {
        "type": "object",
        "properties": {
            "name": {
                "type": "string",
                "description": "list_skills 返回的 Skill 名称。",
            }
        },
        "required": ["name"],
        "additionalProperties": False,
    },
    "strict": False,
}


def list_skills() -> str:
    summaries, errors = SkillLoader().list_skills()
    return json.dumps(
        {
            "skills": [
                {
                    "name": summary.name,
                    "description": summary.description,
                    "path": summary.path,
                }
                for summary in summaries
            ],
            "errors": errors,
        },
        ensure_ascii=False,
    )


def read_skill(name: str) -> str:
    document = SkillLoader().read_skill(name)
    return json.dumps(
        {
            "name": document.name,
            "description": document.description,
            "path": document.path,
            "instructions": document.instructions,
        },
        ensure_ascii=False,
    )


TOOL_DEFINITIONS = [SKILL_LIST_TOOL_DEFINITION, SKILL_READ_TOOL_DEFINITION]
TOOL_HANDLERS = {"list_skills": list_skills, "read_skill": read_skill}
