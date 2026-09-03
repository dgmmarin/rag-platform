# Parsing sidecar (ADR-0006, SPEC-05 §2, STORY-05.3): PDF/DOCX/PPTX/XLSX → blocks.
FROM python:3.11-slim

WORKDIR /app

# Install deps first so the layer caches across app.py edits. All wheels are
# manylinux (PyMuPDF included), so no apt build toolchain is needed.
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY parsers.py app.py ./

# Non-root for parity with the app image (ADR-0002).
RUN useradd --create-home --uid 10001 parser
USER parser

EXPOSE 8081

# gunicorn with a request timeout matching the client-side 120 s parse budget
# (SPEC-05 §2). Threads let a worker handle the compose healthcheck while a slow
# document parses; replicas/processes provide real parallelism (CPU-bound work).
CMD ["gunicorn", "--bind", "0.0.0.0:8081", "--workers", "2", "--threads", "4", \
     "--timeout", "120", "app:app"]
