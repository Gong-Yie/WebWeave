import re
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any


WORKSPACE_ROOT = Path(__file__).resolve().parent.parent
SKILLS_ROOT = (WORKSPACE_ROOT / "data" / "skills").resolve()
SKILL_NAME_PATTERN = re.compile(r"[A-Za-z0-9_-]{1,64}\Z")
FRONT_MATTER_PATTERN = re.compile(
    r"\A---\r?\n(?P<metadata>.*?)\r?\n---(?:\r?\n(?P<body>.*))?\Z",
    re.DOTALL,
)
MAX_SKILL_CHARS = 100_000


@dataclass(frozen=True)
class SkillSummary:
    name: str
    description: str
    path: str


@dataclass(frozen=True)
class SkillDocument:
    name: str
    description: str
    path: str
    instructions: str


class SkillLoader:
    """从 data/skills 目录发现并读取带 YAML front matter 的 Skill。"""

    def __init__(self, root: Path = SKILLS_ROOT) -> None:
        self._root = root.resolve()

    def list_skills(self) -> tuple[list[SkillSummary], list[str]]:
        if not self._root.exists():
            return [], []
        if not self._root.is_dir():
            raise RuntimeError(f"Skill 根目录不是文件夹: {self._root}")

        summaries: list[SkillSummary] = []
        errors: list[str] = []
        names: set[str] = set()
        for skill_dir in sorted(self._root.iterdir(), key=lambda item: item.name):
            if not skill_dir.is_dir() or not SKILL_NAME_PATTERN.fullmatch(
                skill_dir.name
            ):
                continue
            skill_path = skill_dir / "SKILL.md"
            if not skill_path.is_file():
                errors.append(f"{skill_dir.name}: 缺少 SKILL.md")
                continue
            try:
                document = self._read_document(skill_path)
            except (OSError, TypeError, ValueError) as exc:
                errors.append(f"{skill_dir.name}: {exc}")
                continue
            if document.name in names:
                errors.append(f"{skill_dir.name}: Skill 名称重复: {document.name}")
                continue
            names.add(document.name)
            summaries.append(
                SkillSummary(
                    name=document.name,
                    description=document.description,
                    path=document.path,
                )
            )
        return summaries, errors

    def read_skill(self, name: str) -> SkillDocument:
        if not SKILL_NAME_PATTERN.fullmatch(name):
            raise ValueError("Skill 名称无效。")

        summaries, errors = self.list_skills()
        matching = [summary for summary in summaries if summary.name == name]
        if not matching:
            detail = f" 可用 Skill: {', '.join(item.name for item in summaries)}。"
            if errors:
                detail += f" 无效 Skill: {'; '.join(errors)}。"
            raise ValueError(f"未找到 Skill: {name}。{detail}")

        skill_path = self._path_from_summary(matching[0])
        return self._read_document(skill_path)

    def _path_from_summary(self, summary: SkillSummary) -> Path:
        path = (self._root / Path(summary.path)).resolve()
        if self._root not in path.parents or path.name != "SKILL.md":
            raise ValueError("Skill 路径越权。")
        return path

    def _read_document(self, path: Path) -> SkillDocument:
        path = path.resolve()
        if self._root not in path.parents or path.name != "SKILL.md":
            raise ValueError("Skill 路径越权。")
        text = path.read_text(encoding="utf-8")
        if len(text) > MAX_SKILL_CHARS:
            raise ValueError(f"SKILL.md 超过 {MAX_SKILL_CHARS} 个字符。")

        match = FRONT_MATTER_PATTERN.match(text)
        if match is None:
            raise ValueError("SKILL.md 必须包含 YAML front matter。")

        metadata = _parse_front_matter(match.group("metadata"))
        name = metadata.get("name")
        description = metadata.get("description")
        if not isinstance(name, str) or not SKILL_NAME_PATTERN.fullmatch(name):
            raise ValueError("front matter 的 name 必须是合法 Skill 名称。")
        if not isinstance(description, str) or not description.strip():
            raise ValueError("front matter 缺少 description。")

        relative_path = path.relative_to(self._root).as_posix()
        return SkillDocument(
            name=name,
            description=description.strip(),
            path=relative_path,
            instructions=(match.group("body") or "").strip(),
        )


def _parse_front_matter(raw_metadata: str) -> Mapping[str, Any]:
    try:
        import yaml
    except ImportError as exc:
        raise RuntimeError("读取 Skill 需要安装 PyYAML。") from exc

    try:
        metadata = yaml.safe_load(raw_metadata)
    except yaml.YAMLError as exc:
        raise ValueError(f"front matter YAML 无效: {exc}") from exc
    if not isinstance(metadata, Mapping):
        raise ValueError("front matter 必须是 YAML 对象。")
    return metadata
