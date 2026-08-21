# Parsing sidecar stub (STORY-01.2). Real parser deps land in STORY-05.3.
FROM python:3.11-slim

WORKDIR /app
COPY app.py .

# Non-root for parity with the app image (ADR-0002).
RUN useradd --create-home --uid 10001 parser
USER parser

EXPOSE 8081
CMD ["python", "app.py"]
