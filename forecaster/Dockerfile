# ZeroWatch model forecaster (optional sidecar). Build args pick the model,
# which is downloaded at build time so the container starts offline.
#   docker build -t zerowatch-forecaster forecaster/
#   docker run -p 8090:8090 zerowatch-forecaster
# then start zerowatch with --forecaster-url=http://<host>:8090
FROM python:3.11-slim
ARG ZW_MODEL=chronos-bolt-small
ENV ZW_MODEL=${ZW_MODEL} PYTHONUNBUFFERED=1 HF_HOME=/models
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir --extra-index-url https://download.pytorch.org/whl/cpu -r requirements.txt \
 && if [ "$ZW_MODEL" = "timesfm" ]; then pip install --no-cache-dir "timesfm[torch]==3.0.2"; fi
COPY server.py .
RUN if [ "$ZW_MODEL" != "seasonal-naive" ]; then python -c "import server; server.load_backend('${ZW_MODEL}')"; fi
RUN useradd -r -u 10001 zw && chown -R zw /models 2>/dev/null; true
USER 10001
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8090/healthz', timeout=4)"
CMD ["python", "server.py"]
