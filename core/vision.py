import base64
import logging
import mimetypes
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from openai import OpenAI

from .config import VisionProviderConfig, build_openai_client
from .paths import run_directories


logger = logging.getLogger(__name__)
VISION_PROMPT = (
    "请逐张读取用户上传的图片，按文件名分组输出。准确抄录图片中的文字，"
    "并补充主模型完成任务所需的界面、布局、物体和状态信息。看不清的内容明确标注，"
    "不要臆测。只返回纯文本。"
)
IMAGE_MIME_TYPES = {"image/jpeg", "image/png", "image/webp", "image/gif"}


def prepare_user_item(
    model_text: str,
    attachments: Sequence[str],
    run_id: str,
    vision_config: VisionProviderConfig,
) -> dict[str, Any]:
    """将图片附件转为视觉描述，或构造主模型可接收的多模态输入。"""
    image_paths = _image_paths(attachments, run_id)
    if not image_paths:
        return {"role": "user", "content": model_text}

    if vision_config.enabled:
        try:
            description = describe_images(
                build_openai_client(vision_config),
                image_paths,
                vision_config.model,
            )
            return {
                "role": "user",
                "content": (
                    f"{model_text}\n\n[图片视觉描述]\n{description}"
                ),
            }
        except Exception as exc:
            logger.warning("视觉模型处理图片失败，回退主模型多模态输入: %s", exc)

    return {
        "role": "user",
        "content": [
            {"type": "input_text", "text": model_text},
            *(_input_image(path) for path in image_paths),
        ],
    }


def describe_images(
    client: OpenAI,
    image_paths: Sequence[Path],
    model_name: str,
) -> str:
    content: list[dict[str, Any]] = [
        {"type": "input_text", "text": VISION_PROMPT},
    ]
    for path in image_paths:
        content.append(
            {
                "type": "input_text",
                "text": f"图片文件名：{path.name}",
            }
        )
        content.append(_input_image(path))
    response = client.responses.create(
        model=model_name,
        temperature=0.1,
        input=[{"role": "user", "content": content}],
    )
    description = str(getattr(response, "output_text", "") or "").strip()
    if not description:
        raise ValueError("视觉模型没有返回图片文字描述。")
    return description


def _image_paths(attachments: Sequence[str], run_id: str) -> list[Path]:
    resources_root = run_directories(run_id)["resources_dir"].resolve()
    paths: list[Path] = []
    for attachment in attachments:
        candidate = (resources_root / Path(attachment).name).resolve()
        if resources_root not in candidate.parents or not candidate.is_file():
            continue
        content_type = mimetypes.guess_type(candidate.name)[0]
        if content_type in IMAGE_MIME_TYPES:
            paths.append(candidate)
    return paths


def _input_image(path: Path) -> dict[str, Any]:
    content_type = mimetypes.guess_type(path.name)[0] or "application/octet-stream"
    encoded = base64.b64encode(path.read_bytes()).decode("ascii")
    return {
        "type": "input_image",
        "image_url": f"data:{content_type};base64,{encoded}",
        "detail": "auto",
    }
