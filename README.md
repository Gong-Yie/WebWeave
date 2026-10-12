# WebWeave

> **WebWeave** - 让网页生成变得简单高效！

[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![OpenAI SDK](https://img.shields.io/badge/OpenAI%20Go%20SDK-Responses%20API-412991?logo=openai&logoColor=white)](https://developers.openai.com/api/reference/go)
[![License](https://img.shields.io/badge/License-Apache%202.0-D22128)](LICENSE)

## 项目简介

WebWeave 是一个本地运行的智能网页生成工作台。用户只需要用自然语言描述目标，Agent 就会完成需求理解、资料检索、文件生成、命令执行和网页实例预览，并把每一步过程保存在独立的对话运行目录中。

WebWeave 采用 OpenAI 兼容的 Responses API。它适合快速生成网页原型、个人网站、博客、后台页面和交互式前端项目，也适合作为可扩展的本地 Agent 基础工程。

## 技术栈

| 层次 | 技术 | 用途 |
| --- | --- | --- |
| 运行时 | Go 1.26+ | Agent、WebUI 服务和工具执行 |
| 大模型 | 官方 OpenAI Go SDK v3、Responses API | 主模型推理、流式输出和工具调用 |
| 兼容服务 | OpenAI 兼容接口 | 可连接 DeepSeek 或其他兼容服务 |
| 后端 | Go 标准库 `net/http`、`httputil.ReverseProxy` | 本地 HTTP API、静态文件服务和 HTTP/WebSocket 预览代理 |
| 前端 | HTML、CSS、原生 JavaScript | 无构建步骤的工作台界面 |
| 图标 | Lucide | 导航、工具栏和状态图标 |
| Skill | YAML front matter、`go.yaml.in/yaml/v3` | Skill 发现、校验和读取 |
| 持久化 | JSON、JSONL、本地文件目录 | 配置、运行状态、事件、上下文和生成结果 |

## 效果展示

### 完整工作台

工作台同时展示对话过程、工具调用和生成网页实例，用户可以在一次对话中完成生成、验证和修改。

![WebWeave 完整工作台](assert/效果展示.png)

### 控制台与任务入口

控制台采用左侧导航、右侧内容的布局。仪表盘展示全部对话、执行中、已完成和失败任务的实时统计、任务状态分布，以及当前模型、工具和 Skills 概览，并提供新建对话和最近工作入口。历史对话可以从左侧导航继续打开或删除。

![WebWeave 主页](assert/主页展示.png)

### 工具调用状态

执行检查区会记录每次工具调用、耗时、运行状态和事件数量，便于定位生成过程中的问题。

![工具调用状态](assert/工具调用状态.png)

### 生成实例预览

生成结果可以直接在 WebUI 的实例面板中预览，并支持预览、组件选择、定点评价和设备尺寸切换。

![生成实例预览](assert/实例渲染.png)

### 运行文件树

每个对话拥有隔离的资源、结果和下载目录，文件可以在 WebUI 中查看、预览和复制。

![运行文件树](assert/文件树系统.png)

## 项目结构

```text
WebWeave/
├── main.go                       # WebUI 与命令行启动入口
├── core/                         # Agent 与应用核心
│   ├── app.go                    # 后台任务、停止和继续
│   ├── agent.go                  # Responses Agent、视觉模型、上下文压缩
│   ├── config.go                 # config.json 热加载、校验及密钥掩码
│   ├── http.go                   # 前端 API、上传、文件预览和背景图
│   ├── instance.go               # Web 实例生命周期与健康检查
│   ├── paths.go                  # 当前应用的运行目录入口
│   ├── preview.go                # HTTP/WebSocket 预览代理
│   ├── preview_bridge.js         # 组件选择与定点评价桥接
│   ├── preview_bridge.css        # 预览检查样式
│   ├── store.go                  # JSON/JSONL 状态、事件与上下文
│   ├── tools.go                  # 将当前配置交给工具执行器
│   ├── cli.go                    # 命令行对话
│   └── webweave_test.go           # 隔离目录与本地假服务行为测试
├── tools/                        # Agent 工具模块
│   ├── registry.go               # 工具定义、参数、启用状态与超时分发
│   ├── files.go                  # 当前运行内的文件读写
│   ├── search.go                 # 网页检索与文件下载
│   ├── terminal.go               # 终端工具的前台与后台执行
│   ├── process*.go               # PowerShell 与跨平台进程树管理
│   └── *_test.go                 # 文件隔离、检索下载与终端测试
├── skills/
│   └── loader.go                 # YAML Skill 发现、校验与读取
├── workspace/
│   └── paths.go                  # 共享路径边界、运行目录与原子写入
├── webui/                        # 原生 HTML/CSS/JavaScript 工作台
│   ├── index.html
│   ├── app.js
│   └── styles.css
├── data/
│   ├── background/               # WebUI 背景图
│   ├── download/{运行ID}/        # 当前运行下载的资料
│   ├── resources/{运行ID}/       # 当前运行上传的附件
│   ├── result/{运行ID}/          # Agent 生成的项目文件
│   ├── run/{运行ID}/             # 事件、状态和上下文快照
│   └── skills/{名称}/SKILL.md    # 本地 Skill 内容
├── assert/                       # README 效果截图
├── config.json                   # 本地运行配置
├── config.json.example           # 配置模板
├── go.mod                        # Go 模块及依赖版本
├── go.sum                        # Go 依赖校验
├── LICENSE                       # Apache 2.0 许可证
└── README.md
```

## 主要功能

### 1. 自然语言网页生成

Agent 使用 ReAct 循环理解需求，并按需调用工具完成任务。每轮模型响应都可以继续读取工具结果，直到生成结果或向用户询问必要信息。

系统默认提供主Agent、网站灵感设计Agent、代码编程Agent和代码审计Agent。在左侧“Agent 管理”中可编辑角色指令、添加自定义 Agent 和删除自定义 Agent。所有 Agent 共用 `llm.main` 模型配置；定义保存在 `config.json` 的 `agents` 数组，旧配置未包含该字段时自动提供四个默认 Agent。

工作台输入框下方的下拉框默认选择主Agent，可以切换本次任务的执行 Agent。重新打开对话会恢复上次执行的 Agent，停止后继续也会保留选择。主 Agent 可通过 `call_agent` 调用所有专业及自定义 Agent；委派任务共享当前运行的文件目录，使用独立上下文，结果返回主 Agent 后由它继续处理。对话与工具记录显示实际执行的 Agent。默认 Agent 可以编辑，不能删除。

当前内置工具包括：

- `list_skills`、`read_skill`：发现并读取本地 Skill
- `web_search`：检索网页资料并下载文件
- `file_read`、`create_file`：读取输入和生成结果文件
- `terminal_run`：在当前运行目录执行 PowerShell 命令
- `web_instance`：启动、查看、停止和重启生成的网页实例

工具可以在 `config.json` 中独立启用、禁用和设置运行隔离。文件、下载、终端和实例工具需要启用 `run_scoped` 才能访问当前运行目录。搜索与下载直接使用 Go HTTP 客户端，无需安装 curl。

### 2. 对话运行隔离

网页生成优先交付可预览的完整首版；未明确要求多页面时，先完成单页网站。设计方案保持简明，审计只检查影响使用的缺陷，避免反复验证和扩展任务。每次执行的工具轮次预算为：主 Agent 24 轮、设计 Agent 8 轮、审计 Agent 12 轮、编程和自定义 Agent 48 轮。达到预算后会请求一次不调用工具的收尾响应，说明已完成与未完成事项；预算按每次消息或委派计算，不是完成时间保证。

每个对话都有唯一运行 ID，输入资源、生成结果、下载文件、事件日志、状态和上下文互相隔离。对话支持：

- 流式显示模型输出
- 协作式停止和停止后继续
- 历史记录重新打开
- 对话记录删除（保留附件、下载及生成文件，与原后端行为一致）
- 运行事件和工具调用追踪

### 3. 可选视觉模型

图片附件可以直接交给主模型，也可以启用独立视觉模型先生成文字描述，再交给主模型处理：

- `llm.vision.enabled=false`：主模型直接识别图片
- `llm.vision.enabled=true`：视觉模型负责图片转文字，主模型负责任务推理
- 视觉模型调用失败时，会回退为主模型的多模态输入

### 4. 生成实例与检查

Agent 生成网页后，可以通过实例面板直接查看本地预览。检查区提供：

- 预览、组件选择和定点评价模式
- 桌面、平板和手机尺寸切换
- 同源 HTTP/WebSocket 预览代理
- 实例刷新、重启、停止和新窗口打开

### 5. 文件树与结果预览

文件面板会按 `resources`、`result` 和 `download` 展示运行目录，支持文本预览、图片预览和内容复制。生成项目默认写入 `data/result/{运行ID}`。

### 6. 设置、主题和 Skill 管理

左侧导航提供运行配置、模型配置和外观设置三个独立入口：

- 运行时模型、工具、超时和上下文压缩配置
- 主模型和可选视觉模型配置
- 主题切换和背景图上传
- API Key 在界面中以 `********` 掩码显示
- 左侧 Skills 页面展示 `data/skills` 下可用 Skill

背景图会保存到 `data/background`，不需要额外的静态资源构建步骤。

### 7. 上下文压缩与热加载

模型上下文会缩短大型工具结果（正文预览最多 12,000 字符）和已成功写入的大型文件内容；完整结果仍保存在 `events.jsonl`，源码保存在结果文件中。压缩请求的历史片段限制为 24,000 字符，保留最初需求、最近用户消息和完整工具调用配对。压缩失败时保留原需求及已有摘要，并在 `context_compressed.compression_error` 中记录原因。后续保存上下文会继续保留摘要元数据。

配置模板的 `context.max_input_tokens` 调整为 65,536，避免等到数十万 Token 才触发压缩。Token 数仍是程序估算值，并非模型提供商的精确计费值。`file_read` 的普通相对路径默认指向当前运行的 result 目录，也支持显式 scope、项目 `data` 路径和允许的绝对路径。

上下文达到 `config.json` 中的阈值后，Agent 会生成摘要并保留最近项目，降低长对话的输入成本。配置文件与 `data/skills` 中的 SKILL 文件支持运行时重新读取。工具定义和分发位于 `tools/registry.go`，文件、搜索与终端实现分别位于对应模块文件；修改 Go 源码后需要重新编译或重新执行 `go run`。

代码依赖方向为 `main → core → tools / skills / workspace`，`tools` 依赖 `skills` 和 `workspace`，`skills` 依赖 `workspace`。工具通过应用注入的回调执行实例操作，不导入 `core`，避免循环依赖。`skills/` 保存加载器源码，`data/skills/` 保存用户的 SKILL 文档。

### 8. 从 Python 后端迁移

后端不再需要 Python。已有 `config.json`、`data/run/{运行ID}/state.json`、`events.jsonl`、`context.json` 和资源目录可继续使用，前端 API 路径保持一致。停止旧 Python 服务后再启动 Go 服务，以避免端口占用和两个进程同时修改运行数据。

旧对话恢复时会移除未配对的工具调用与结果。已经退出的历史实例需要重新启动；如果原启动命令本身使用 Python，则该生成项目仍需要 Python，这与 WebWeave 后端运行时无关。

## 安装指南

### 环境要求

- Go 1.26 或更高版本
- 一个 OpenAI 兼容的大模型服务和 API Key
- Windows、Linux 或 macOS；终端工具和实例启动需要 PowerShell（Windows 内置 PowerShell 或安装 `pwsh`）

### 1. 获取项目

```bash
git clone https://github.com/Gong-Yie/WebWeave.git
cd WebWeave
```

### 2. 下载依赖

```powershell
go mod download
```

### 3. 配置大模型

首次安装且项目根目录没有 `config.json` 时，可以复制配置模板：

```powershell
Copy-Item config.json.example config.json
```

编辑 `config.json` 的 `llm` 节点：

```json
{
  "llm": {
    "main": {
      "model": "your-main-model",
      "base_url": "https://api.example.com/v1",
      "api_key": "your-main-api-key"
    },
    "vision": {
      "enabled": false,
      "model": "",
      "base_url": "",
      "api_key": ""
    }
  }
}
```

`llm.main` 是主模型配置。视觉模型是可选的，只有在 `enabled` 为 `true` 时才需要填写完整的模型、地址和 Key。LLM 配置统一保存在 `config.json`，不再从 `.env` 读取。请勿将真实 API Key 提交到 Git 仓库。

其他运行参数包括：

- `model.stream`：是否流式输出
- `tools.<name>.enabled`：是否向模型暴露工具
- `tools.<name>.run_scoped`：是否允许工具访问当前运行的隔离目录
- `tool_timeout`：工具默认和最大超时
- `context`：上下文压缩开关、阈值和保留项目数

也可以直接在 WebUI 左侧的“模型配置”和“运行配置”页面修改，配置会被校验并原子写入。

模型连接遇到超时、连接中断、限流或服务端错误时会自动重试，最多 5 次；请求参数错误不会重试。

WebUI 设置页不内置模型、超时或工具开关的运行参数；配置控件在成功读取当前 `config.json` 后才启用，保存内容由该配置文件统一管理。

## 使用方法

### 1. 启动 WebUI

```powershell
go run .
```

默认地址为：

<http://127.0.0.1:8765>

自定义端口：

```powershell
go run . --host 127.0.0.1 --port 8765
```

服务默认只监听本机地址，适合本地使用。启动后在浏览器打开地址即可进入 WebWeave 工作台。

也可以构建后启动（Windows PowerShell）：

```powershell
go build -o webweave.exe .
.\webweave.exe
```

从其他目录运行时，使用 `--root` 指定包含 `webui`、`config.json` 和 `data` 的项目根目录。Linux/macOS 构建时可使用 `go build -o webweave .`。

命令行对话及恢复已有对话：

```powershell
go run . --cli
go run . --cli --run-id 现有运行ID
```

### 2. 完成一次网页任务

1. 点击“新建对话”或左侧“工作台”。
2. 描述网页目标、页面结构、视觉风格和交互要求。
3. 通过附件按钮上传参考图、需求文档或其他资源。
4. 观察对话区和执行检查区中的工具调用过程。
5. 在“实例”面板预览生成结果，必要时使用组件选择或定点评价。
6. 在同一对话中继续提出修改要求，Agent 会使用已有上下文继续工作。
7. 在“文件”面板检查生成文件，最终项目位于 `data/result/{运行ID}`。

### 3. 使用 Skills

在 `data/skills` 下新增一个目录，并创建带 front matter 的 `SKILL.md`：

```text
data/skills/my_skill/SKILL.md
```

```markdown
---
name: my_skill
description: 描述这个 Skill 解决什么问题。
---

# 使用说明

在这里写给 Agent 的具体工作规则、检查步骤和输出要求。
```

刷新 WebUI 的 Skills 页面后，Agent 也可以通过 `list_skills` 和 `read_skill` 发现并读取它。

### 4. 管理主题和背景图

进入左侧“外观设置”选择主题或上传 PNG、JPG、WebP 背景图。背景图会保存到 `data/background`，删除背景不会影响对话和生成结果。

### 5. 本地 API

WebUI 同时提供用于前端和自动化集成的本地 API：

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| `GET` | `/api/health` | 检查服务状态 |
| `GET` / `PUT` | `/api/config` | 读取或保存运行配置 |
| `GET` | `/api/skills` | 获取本地 Skill 列表 |
| `GET` / `POST` | `/api/agents` | 列出 Agent 或添加自定义 Agent |
| `PUT` / `DELETE` | `/api/agents/{agent_id}` | 编辑 Agent 或删除自定义 Agent |
| `GET` / `POST` | `/api/runs` | 列出或创建对话 |
| `DELETE` | `/api/runs/{run_id}` | 删除对话记录，保留资源与生成文件 |

`POST /api/runs/{run_id}/messages` 支持 `agent_id`，省略时使用主Agent。`POST /api/runs/{run_id}/continue` 可传入 `agent_id` 切换执行者，空请求体则沿用对话中保存的 Agent。

## 贡献指南

欢迎通过 Issue、Pull Request 或 Skill 提交改进建议。建议遵循以下流程：

1. 先创建 Issue，说明问题、复现步骤或功能目标。
2. 从最新代码创建独立分支，保持一次提交只解决一个主题。
3. 修改前阅读相关模块和配置，尽量复用现有工具链和目录边界。
4. 新增行为时补充直接相关的测试或可复现验证步骤。
5. 提交前运行最小必要检查：

   ```powershell
   go test ./...
   ```

6. 不要提交真实 API Key、运行生成物、个人背景图或临时调试文件。
7. Pull Request 中请说明改动范围、验证命令和已知限制。

测试使用系统临时目录和本地假模型 HTTP 服务，不连接真实模型提供商。实例生命周期测试会通过 PowerShell 启动并清理本地测试服务。前端代码修改时另行执行 `node --check webui/app.js`。

## 许可证

WebWeave 采用 [Apache License 2.0](LICENSE) 开源。使用、修改和再分发本项目时，请保留许可证和相关版权声明。


---

**WebWeave** - 让网页生成变得简单高效！
