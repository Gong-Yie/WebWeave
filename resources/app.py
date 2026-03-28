from flask import Flask, jsonify, request

app = Flask(__name__)

# 首页路由
@app.route('/')
def home():
    return "欢迎访问 Flask 后端！"

# GET 示例：返回 JSON 数据
@app.route('/api/hello')
def hello():
    return jsonify({"message": "Hello, World!"})

# POST 示例：接收 JSON 请求并返回响应
@app.route('/api/data', methods=['POST'])
def handle_data():
    data = request.get_json()
    # 假设接收到的数据包含 'name' 字段
    name = data.get('name', '匿名用户')
    return jsonify({"greeting": f"你好，{name}！"})

# 动态路由：根据 URL 参数返回用户信息
@app.route('/user/<username>')
def user_profile(username):
    return jsonify({"username": username, "status": "active"})

if __name__ == '__main__':
    # 开启调试模式，便于开发
    app.run(debug=True)