# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository Structure

This is a research monorepo. Each top-level folder is an independent project exploring a specific technology, problem, or idea. Projects are self-contained and may use entirely different languages, frameworks, and tooling from one another.

## Working in This Repo

- Treat each subfolder as its own isolated project. Check for a local README, package.json, Makefile, pyproject.toml, Cargo.toml, or equivalent before running any commands.
- Build, test, and lint commands vary per project — always discover them from the project's own config files rather than assuming.
- When adding a new research project, create a new top-level folder. Include a README.md in it describing the goal, the technology being explored, and how to run it.
- There is no shared dependency management or build system across projects.

## Python Projects

- All Python projects must use **uv** for dependency and environment management.
- Use `pyproject.toml` for project metadata and dependencies — never `requirements.txt`.
- Install dependencies with `uv sync`. Run scripts with `uv run python <script>` (or activate the venv with `source .venv/bin/activate` first).
- Add dependencies with `uv add <package>`, dev-only dependencies with `uv add --dev <package>`.
