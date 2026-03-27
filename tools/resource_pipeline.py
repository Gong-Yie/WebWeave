import os
import json
from pathlib import Path
from typing import Dict, List, Any
from langchain_core.tools import tool
from langchain_openai import ChatOpenAI

@tool
def scan_resources_source(resources_dir: str = "") -> str:
    """扫描 resources 目录下的源码，返回文件清单和关键片段"""
    resources_path = Path(resources_dir) if resources_dir else Path(__file__).parent.parent / "resources"
    
    if not resources_path.exists():
        return "resources 目录不存在"
    
    files = []
    for root, _, filenames in os.walk(resources_path):
        for filename in filenames:
            file_path = Path(root) / filename
            relative_path = file_path.relative_to(resources_path)
            
            try:
                with open(file_path, 'r', encoding='utf-8', errors='ignore') as f:
                    content = f.read()
                # 提取前200字符作为预览
                preview = content[:200].strip() + ("..." if len(content) > 200 else "")
                files.append({
                    "path": str(relative_path),
                    "size": len(content),
                    "preview": preview
                })
            except Exception as e:
                files.append({
                    "path": str(relative_path),
                    "error": str(e)
                })
    
    return json.dumps({
        "resources_dir": str(resources_path),
        "file_count": len(files),
        "files": files
    }, ensure_ascii=False, indent=2)

@tool
def analyze_resources_stack(resources_summary: str) -> str:
    """分析 resources 中的源码，识别技术栈并判断是前端/后端/全栈"""
    llm = ChatOpenAI(
        model="deepseek-chat",
        temperature=0.3,
        api_key=os.getenv("DEEPSEEK_API_KEY"),
        base_url="https://api.deepseek.com/v1",
    )
    
    prompt = f"""
请分析以下资源文件，识别技术栈并判断是前端、后端还是全栈项目：

{resources_summary}

分析要求：
1. 识别使用的主要编程语言
2. 识别使用的框架和库
3. 判断是前端、后端还是全栈项目
4. 给出简要的技术栈总结
"""
    
    response = llm.invoke(prompt)
    return response.content

@tool
def propose_build_questions(tech_stack_analysis: str) -> str:
    """基于技术栈分析结果，提出需要向用户澄清的问题"""
    llm = ChatOpenAI(
        model="deepseek-chat",
        temperature=0.7,
        api_key=os.getenv("DEEPSEEK_API_KEY"),
        base_url="https://api.deepseek.com/v1",
    )
    
    prompt = f"""
基于以下技术栈分析结果，提出需要向用户澄清的问题：

{tech_stack_analysis}

提问要求：
1. 如果是前端项目，询问后端技术栈和数据接口需求
2. 如果是后端项目，询问前端技术栈和页面风格需求
3. 询问网站的功能需求和设计风格
4. 询问是否需要使用特定的模板或主题
5. 询问部署和性能要求
"""
    
    response = llm.invoke(prompt)
    return response.content

@tool
def generate_project_structure(requirements: str, tech_stack: str) -> str:
    """根据用户需求和技术栈，生成完整的项目结构"""
    llm = ChatOpenAI(
        model="deepseek-chat",
        temperature=0.1,
        api_key=os.getenv("DEEPSEEK_API_KEY"),
        base_url="https://api.deepseek.com/v1",
    )
    
    prompt = f"""
根据以下用户需求和技术栈，生成完整的项目结构。

用户需求：
{requirements}

技术栈：
{tech_stack}

请严格按照以下JSON格式返回项目结构，不要包含任何其他文字说明：

{{
  "project_name": "项目名称",
  "description": "项目描述",
  "files": [
    {{
      "path": "文件相对路径（如：app.py, static/css/style.css）",
      "description": "文件描述"
    }}
  ]
}}

重要要求：
1. 必须返回有效的JSON格式
2. files数组必须包含所有需要生成的文件，不要省略任何文件
3. 文件路径使用正斜杠（/）分隔
4. 不要包含任何JSON之外的文字说明
5. 确保项目结构合理且完整
6. 不要使用"..."或其他省略符号
7. 列出所有具体的文件路径，不要使用"其他文件"等模糊描述
"""
    
    response = llm.invoke(prompt)
    return response.content

def _generate_file_content(file_path: str, project_structure: str, requirements: str, tech_stack: str) -> str:
    """根据项目结构和需求，生成指定文件的内容"""
    llm = ChatOpenAI(
        model="deepseek-chat",
        temperature=0.3,
        api_key=os.getenv("DEEPSEEK_API_KEY"),
        base_url="https://api.deepseek.com/v1",
    )
    
    prompt = f"""
根据以下信息，生成 {file_path} 文件的完整内容。

项目结构：
{project_structure}

用户需求：
{requirements}

技术栈：
{tech_stack}

重要要求：
1. 只生成文件的实际代码内容，不要包含任何markdown代码块标记（```）
2. 不要包含任何解释性文字、说明或注释
3. 不要包含"文件说明"、"使用方法"等额外内容
4. 生成完整的、可运行的代码
5. 确保代码符合最佳实践
6. 代码要使用中文注释
7. 直接开始文件内容，不要有任何前导文字
8. 确保文件内容完整，不要在中间截断

文件类型：{file_path.split('.')[-1] if '.' in file_path else 'unknown'}
"""
    
    response = llm.invoke(prompt)
    return response.content

@tool
def generate_file_content(file_path: str, project_structure: str, requirements: str, tech_stack: str) -> str:
    """根据项目结构和需求，生成指定文件的内容"""
    return _generate_file_content(file_path, project_structure, requirements, tech_stack)

def _generate_readme(project_structure: str, requirements: str, tech_stack: str) -> str:
    """生成项目的 README.md 文件内容"""
    llm = ChatOpenAI(
        model="deepseek-chat",
        temperature=0.3,
        api_key=os.getenv("DEEPSEEK_API_KEY"),
        base_url="https://api.deepseek.com/v1",
    )
    
    prompt = f"""
根据以下信息，生成项目的 README.md 文件内容。

项目结构：
{project_structure}

用户需求：
{requirements}

技术栈：
{tech_stack}

重要要求：
1. 只生成README.md的实际内容，不要包含任何markdown代码块标记（```）
2. 不要包含任何解释性文字、说明或注释
3. 直接开始README内容，以#标题开始
4. 使用中文编写
5. 包含项目介绍
6. 包含技术栈说明
7. 包含安装和运行说明
8. 包含项目结构说明
9. 包含功能特性说明
10. 确保内容完整，不要在中间截断
"""
    
    response = llm.invoke(prompt)
    return response.content

@tool
def generate_readme(project_structure: str, requirements: str, tech_stack: str) -> str:
    """生成项目的 README.md 文件内容"""
    return _generate_readme(project_structure, requirements, tech_stack)

@tool
def build_web_project_from_resources(
    requirements: str,
    tech_stack: str,
    project_structure: str,
    result_dir: str = ""
) -> str:
    """根据需求、技术栈和项目结构，生成完整的网页项目并保存到 result 目录"""
    result_path = Path(result_dir) if result_dir else Path(__file__).parent.parent / "result"
    result_path.mkdir(exist_ok=True, parents=True)
    
    # 解析JSON格式的项目结构
    try:
        # 清理可能的markdown代码块标记
        cleaned_json = project_structure.strip()
        if cleaned_json.startswith('```json'):
            cleaned_json = cleaned_json[7:]
        if cleaned_json.startswith('```'):
            cleaned_json = cleaned_json[3:]
        if cleaned_json.endswith('```'):
            cleaned_json = cleaned_json[:-3]
        cleaned_json = cleaned_json.strip()
        
        # 解析JSON
        structure_data = json.loads(cleaned_json)
        files = [file_info["path"] for file_info in structure_data.get("files", [])]
    except json.JSONDecodeError as e:
        # 如果JSON解析失败，尝试使用LLM提取
        print(f"JSON解析失败: {e}，尝试使用LLM提取文件列表")
        llm = ChatOpenAI(
            model="deepseek-chat",
            temperature=0.3,
            api_key=os.getenv("DEEPSEEK_API_KEY"),
            base_url="https://api.deepseek.com/v1",
        )
        
        extract_prompt = f"""
从以下项目结构中提取所有文件路径，返回一个 JSON 数组：

{project_structure}

只返回文件路径，不要包含目录路径。格式如下：
["app.py", "static/css/style.css", "templates/index.html"]
"""
        
        files_response = llm.invoke(extract_prompt)
        try:
            files = json.loads(files_response.content)
        except:
            # 如果解析失败，尝试手动提取
            files = []
            lines = project_structure.split('\n')
            for line in lines:
                line = line.strip()
                if line and not line.endswith('/') and not line.startswith(' ') and '.' in line:
                    # 移除可能的引号和标记
                    cleaned_line = line.replace('"', '').replace('\'', '').replace('`', '')
                    if cleaned_line and not cleaned_line.startswith('#'):
                        files.append(cleaned_line)
    
    # 生成每个文件
    generated_files = []
    print(f"准备生成 {len(files)} 个文件")
    for file_path in files:
        try:
            # 清理文件路径
            cleaned_path = file_path.strip()
            # 移除引号和其他无效字符
            cleaned_path = cleaned_path.replace('"', '').replace('\'', '').replace('`', '')
            # 跳过非文件路径（如目录或格式错误的路径）
            if not cleaned_path or cleaned_path.endswith('/') or '```' in cleaned_path or cleaned_path.startswith('[') or cleaned_path.startswith(']'):
                print(f"跳过无效路径: {cleaned_path}")
                continue
            
            print(f"正在生成文件: {cleaned_path}")
            content = _generate_file_content(cleaned_path, project_structure, requirements, tech_stack)
            
            # 清理内容：移除markdown代码块标记和多余说明
            content = content.strip()
            
            # 移除markdown代码块标记
            if content.startswith('```'):
                # 找到第一个换行符
                first_newline = content.find('\n')
                if first_newline != -1:
                    content = content[first_newline + 1:]
                # 移除结尾的代码块标记
                if content.endswith('```'):
                    content = content[:-3].strip()
            
            # 移除多余的说明性文字（在代码之前的文字）
            lines = content.split('\n')
            code_start = 0
            for i, line in enumerate(lines):
                # 如果是代码文件，找到代码开始的位置
                if cleaned_path.endswith(('.py', '.js', '.html', '.css', '.json')):
                    # 跳过说明性文字
                    if line.strip().startswith(('```', '#', '//', '/*', '*', '文件说明', '使用说明', '这个', '该', '此')):
                        continue
                    else:
                        code_start = i
                        break
                else:
                    code_start = i
                    break
            
            if code_start > 0:
                content = '\n'.join(lines[code_start:])
            
            # 检查文件完整性
            file_ext = cleaned_path.split('.')[-1] if '.' in cleaned_path else ''
            
            # Python文件检查
            if file_ext == 'py':
                # 检查是否有未闭合的括号或引号
                if not content.rstrip().endswith('\n'):
                    content += '\n'
                # 简单的完整性检查
                open_parens = content.count('(') - content.count(')')
                open_brackets = content.count('[') - content.count(']')
                open_braces = content.count('{') - content.count('}')
                
                if open_parens > 0 or open_brackets > 0 or open_braces > 0:
                    print(f"警告: 文件 {cleaned_path} 可能不完整，括号未闭合")
            
            # HTML文件检查
            elif file_ext in ['html', 'htm']:
                # 检查是否是Jinja2模板（继承base.html）
                if '{% extends' in content or '{% block' in content:
                    # Jinja2模板，检查是否正确结束
                    if not ('{% endblock %}' in content or '{% endblock' in content):
                        print(f"警告: HTML文件 {cleaned_path} 可能不完整，缺少endblock")
                else:
                    # 普通HTML文件，检查是否以</html>结尾
                    if not content.rstrip().endswith('</html>'):
                        print(f"警告: HTML文件 {cleaned_path} 可能不完整")
            
            # JSON文件检查
            elif file_ext == 'json':
                try:
                    json.loads(content)
                except json.JSONDecodeError as e:
                    print(f"警告: JSON文件 {cleaned_path} 格式错误: {e}")
            
            # 保存文件
            full_path = result_path / cleaned_path
            full_path.parent.mkdir(exist_ok=True, parents=True)
            with open(full_path, 'w', encoding='utf-8') as f:
                f.write(content)
            generated_files.append(str(full_path))
            print(f"成功生成文件: {cleaned_path} (长度: {len(content)} 字符)")
        except Exception as e:
            print(f"生成文件 {file_path} 失败: {e}")
            import traceback
            traceback.print_exc()
    
    # 生成 README.md
    try:
        readme_content = _generate_readme(project_structure, requirements, tech_stack)
        
        # 清理README内容
        readme_content = readme_content.strip()
        
        # 移除markdown代码块标记
        if readme_content.startswith('```'):
            first_newline = readme_content.find('\n')
            if first_newline != -1:
                readme_content = readme_content[first_newline + 1:]
            if readme_content.endswith('```'):
                readme_content = readme_content[:-3].strip()
        
        # 确保README以#开头
        lines = readme_content.split('\n')
        for i, line in enumerate(lines):
            if line.strip().startswith('#'):
                readme_content = '\n'.join(lines[i:])
                break
        
        with open(result_path / "README.md", 'w', encoding='utf-8') as f:
            f.write(readme_content)
        generated_files.append(str(result_path / "README.md"))
        print(f"成功生成文件: README.md (长度: {len(readme_content)} 字符)")
    except Exception as e:
        print(f"生成 README.md 失败: {e}")
        import traceback
        traceback.print_exc()
    
    return json.dumps({
        "result_dir": str(result_path),
        "generated_files": generated_files,
        "file_count": len(generated_files)
    }, ensure_ascii=False, indent=2)