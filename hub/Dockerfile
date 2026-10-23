FROM python:3.13-slim

WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY server.py .
RUN useradd --system --uid 10001 clip && mkdir /data && chown clip /data

USER clip
ENV CLIP_DATA=/data CLIP_PORT=8766 PYTHONUNBUFFERED=1
VOLUME /data
EXPOSE 8766

HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD python -c "import urllib.request as u; u.urlopen('http://127.0.0.1:8766/healthz', timeout=2)"

CMD ["python", "server.py"]
