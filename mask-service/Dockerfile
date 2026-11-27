# 敏感数据识别服务。
#
# FastAPI 是纯 Python，镜像小、启动快——这个服务只做正则 + 校验位，
# 不需要任何模型或重型依赖。

FROM python:3.12-slim

WORKDIR /app

# 依赖先装，利用构建缓存：只改业务代码时不用重装依赖
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY main.py .
# 单元测试也带上：镜像里能直接跑 pytest，验证不依赖本机 Python
COPY test_redact.py .

# 非 root 运行
RUN useradd -m -u 10001 appuser
USER appuser

EXPOSE 8021

# 用 uvicorn 直接跑，不加 gunicorn：单进程足够，
# 这个服务是无状态的，需要扩容时起多个副本即可。
CMD ["uvicorn", "main:app", "--host", "0.0.0.0", "--port", "8021", "--log-level", "info"]
