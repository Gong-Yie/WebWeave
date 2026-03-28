## 项目简介

WebWeave 是一个基于 LangChain 框架开发的智能网页生成代理，能够根据用户提供的需求和技术栈，自动生成完整的网页项目结构和代码。该代理具有以下核心功能：

- 读取用户提供的资源文件，分析技术栈
- 生成完整的项目结构，包括前端和后端文件
- 为每个文件生成高质量的代码内容
- 支持多种技术栈，包括 Flask、Django、React 等
- 提供项目README.md文件生成功能

## 技术栈

[![Python 3.10+](https://img.shields.io/badge/Python-3.10%2B-blue.svg)](https://www.python.org/)
[![LangChain](https://img.shields.io/badge/LangChain-0.2.0-green.svg)](https://github.com/langchain-ai/langchain)
[![License](https://img.shields.io/badge/License-Apache%202.0-red.svg)](https://www.apache.org/licenses/LICENSE-2.0)

## 项目结构

```
WebWeave/
├── tools/             # 工具模块
│   ├── resource_pipeline.py  # 资源处理流程
│   └── web_search.py  # 网络搜索工具
├── core.py            # 核心逻辑
├── requirements.txt   # 依赖文件
├── .env               # 环境变量配置
└── README.md          # 项目说明
```

## 主要功能

### 1. 资源分析
- 扫描 resources 目录中的源码文件
- 识别技术栈和前后端分类
- 提取关键信息用于后续生成

### 2. 项目结构生成
- 根据用户需求和技术栈，生成完整的项目结构
- 确保所有必要文件都被包含，无遗漏
- 以 JSON 格式输出，确保结构清晰

### 3. 文件内容生成
- 为每个文件生成高质量的代码内容
- 确保代码符合行业标准和最佳实践
- 支持多种文件类型：HTML、CSS、JavaScript、Python 等

### 4. 内容清理和验证
- 移除多余的 Markdown 代码块和注释
- 验证文件完整性，确保代码结构正确
- 处理特殊文件类型的验证逻辑

### 5. 项目文档生成
- 为生成的项目创建详细的 README.md 文件
- 包含项目介绍、技术栈、功能特性等信息

## 安装指南

### 1. 克隆项目

```bash
git clone git@github.com:Gong-Yie/WebWeave.git
cd WebWeave
```

### 2. 创建虚拟环境（可选）

```bash
python -m venv web
```

#### 激活

windows

```bash
web\Scripts\activate
```

linux

```bash
source web/bin/activate
```


### 3. 安装依赖

```bash
python.exe -m pip install --upgrade pip
pip install -r requirements.txt
```

### 4. 配置环境变量

在 `.env` 文件中配置以下环境变量：

```
# .env 文件
DEEPSEEK_API_KEY=your_api_key_here
TAVILY_API_KEY=your_api_key_here(可选)
```

## 使用方法

### 1. 创建resources（如果没有）

然后把idea放在resources中

```
WebWeave/
└── resources/
    └── 你的网页idea
```

### 2. 启动代理

```bash
python core.py
```

### 3. 交互流程

1. **资源分析**：代理会自动扫描 resources 目录中的文件
2. **技术栈识别**：大模型会分析技术栈和前后端分类
3. **需求澄清**：代理会询问用户关于网站的具体要求
4. **项目生成**：根据用户需求生成完整的项目结构和代码
5. **结果查看**：生成的代码会保存在 `result` 目录中

### 4. 自定义配置

可以通过修改 `core.py` 中的参数来调整代理的行为：

- `temperature`：控制生成内容的随机性
- `model`：选择使用的大语言模型
- `base_url`：配置模型 API 地址

## 输出结果

生成的项目会保存在 `result` 目录中，包含：

- 完整的项目结构
- 所有必要的代码文件
- 项目 README.md 文档
- 静态资源文件

## 示例输出

### 生成的项目结构示例

flask示例


```
result/
├── app.py            # 主应用文件
├── config.py         # 配置文件
├── requirements.txt  # 依赖文件
├── static/           # 静态资源
│   ├── css/          # CSS 文件
│   ├── js/           # JavaScript 文件
│   └── images/       # 图片文件
├── templates/        # 模板文件
│   ├── base.html     # 基础模板
│   ├── index.html    # 首页
│   └── other pages   # 其他页面
└── README.md         # 项目说明
```

## 贡献指南

欢迎提交 Issue 和 Pull Request 来改进这个项目。

## 许可证

本项目采用 Apache 2.0 开源许可证。详见 [LICENSE](LICENSE) 文件。

## 联系方式

如有问题或建议，请联系项目维护者。

---

** WebWeave** - 让网页生成变得简单高效！
