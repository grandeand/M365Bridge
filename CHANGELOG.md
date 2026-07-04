# Changelog

All notable changes to M365Bridge will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.1] - 2026-07-05

### Added
- Docker support with multi-stage Dockerfile and docker-compose.yml
- GitHub Actions CI workflow (cross-platform build for linux/darwin/windows amd64+arm64)
- GitHub Actions release workflow (6 platform binaries + multi-arch Docker image push to ghcr.io)
- Pre-built binary downloads from GitHub Releases
- .dockerignore for optimized Docker build context
- Version update skill for automated version bumping
- Prerequisites section and first-run expectations in README
- Model selection guide in README
- Anthropic SDK and image input Python examples in README
- .env example format in README

### Changed
- Project renamed from m365-copilot2api to M365Bridge
- Go module path changed to github.com/KilimcininKorOglu/M365Bridge
- Binary output moved to bin/ directory
- Encryption key storage moved from ~/.m365-copilot/ to data/tokens/
- .env file location moved from project root to data/.env
- Setup wizard output messages updated to use ./bin/m365-bridge paths
- Version field changed from const to var for ldflags override
- Version output changed from "M365 Copilot CLI" to "M365Bridge"
- README badges and Docker pull instructions added
- .gitignore updated for new project structure
