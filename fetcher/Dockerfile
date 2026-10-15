FROM python:3.12-slim

RUN pip install --no-cache-dir "flask>=3.0" "requests>=2.32" "gdown>=5.2" "gunicorn>=22"

WORKDIR /app
COPY app.py .

ENV DATA_DIR=/data
USER 1000:1000
EXPOSE 8000

# One worker so all downloads share one job list; threads handle the page and downloads.
CMD ["gunicorn", "--workers", "1", "--threads", "16", "--bind", "0.0.0.0:8000", "app:app"]
