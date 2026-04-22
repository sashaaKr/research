# Build context: temporal-research/
#
#   docker build -f 09_k8s/app/Dockerfile -t temporal-worker:latest .
#
# The context is the parent directory so we can copy shared code
# from 08_template_runner alongside the worker entrypoint.
# In a real project this shared code would be an installable package.

FROM python:3.12-slim

WORKDIR /app

# Install uv from the official image — faster than pip for dependency resolution
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

# Install dependencies first (cached layer — only re-runs when pyproject.toml changes)
COPY 09_k8s/app/pyproject.toml ./
RUN uv sync --frozen --no-dev --no-install-project 2>/dev/null || uv sync --no-dev --no-install-project

# Copy shared workflow + activity code from the sibling example
# In production: replace this with `pip install your-workflow-package`
COPY 08_template_runner/models.py      ./models.py
COPY 08_template_runner/activities.py  ./activities.py

# Copy the K8s-specific interpreter (adds activity_task_queue routing)
COPY 09_k8s/app/interpreter.py         ./interpreter.py
COPY 09_k8s/app/worker.py              ./worker.py

ENV PATH="/app/.venv/bin:$PATH"

# Non-root user — security best practice
RUN useradd --no-create-home --shell /bin/false worker
USER worker

# WORKER_TYPE is overridden per-Deployment in the K8s manifests
ENV WORKER_TYPE=all

CMD ["python", "worker.py"]
