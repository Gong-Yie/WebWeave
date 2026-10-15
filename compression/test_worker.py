from __future__ import annotations

import io
import json
import unittest
from unittest.mock import patch

from worker import CompressionRequest, compress, serve


class FakeCompressor:
    def compress_prompt(self, context: str, **kwargs: object) -> dict[str, object]:
        self.last_rate = kwargs["rate"]
        return {"compressed_prompt": context.replace("冗余背景描述", "背景")}


class WorkerTests(unittest.TestCase):
    @patch("worker.count_tokens", side_effect=len)
    def test_preserves_constraints_and_exact_values(self, _counter: object) -> None:
        code = "```js\nconst title = '冗余背景描述';\nsubmit(form);\n```\n"
        text = (
            "冗余背景描述用于营造氛围。\n必须使用中文，不得增加页面。\n"
            "宽度 1280，文件 index.html。\n" + code + "冗余背景描述再次介绍风格。"
        )
        model = FakeCompressor()
        result = compress(CompressionRequest(text, 0.7), model)
        self.assertTrue(result.ok)
        self.assertLess(result.output_tokens, result.input_tokens)
        self.assertIn("必须使用中文，不得增加页面。\n宽度 1280，文件 index.html。", result.text)
        self.assertEqual(model.last_rate, 0.7)
        self.assertIn(code, result.text)

    @patch("worker.count_tokens", side_effect=len)
    def test_protocol_survives_bad_requests(self, _counter: object) -> None:
        invalid = [
            "[]", '{"text":"内容","rate":true}',
            '{"text":"内容","rate":0}', '{"text":"内容","rate":NaN}',
        ]
        valid = json.dumps({"text": "冗余背景描述", "rate": 0.7})
        source = io.StringIO("\n".join(invalid + [valid]) + "\n")
        target = io.StringIO()
        with self.assertLogs("worker", level="ERROR"):
            serve(source, target, FakeCompressor())
        replies = [json.loads(line) for line in target.getvalue().splitlines()]
        self.assertEqual(
            [reply["ok"] for reply in replies], [False] * len(invalid) + [True]
        )

    @patch("worker.count_tokens", side_effect=len)
    def test_fully_protected_text_is_unchanged(self, _counter: object) -> None:
        text = "必须保留 768px 的移动端布局。"
        result = compress(CompressionRequest(text, 1), FakeCompressor())
        self.assertEqual(result.text, text)
        self.assertEqual(result.input_tokens, result.output_tokens)


if __name__ == "__main__":
    unittest.main()
