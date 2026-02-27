---
title: "feat: Add GitHub Actions CI/CD workflows and complete .github setup"
labels: ["kind/feature", "ci/cd"]
---

## Description

This PR adds complete GitHub Actions CI/CD workflows following the kagenti-operator repository pattern, along with all supporting `.github` configuration files.

## Changes Made

### GitHub Workflows

1. **CI Workflow** ([`.github/workflows/ci.yaml`](.github/workflows/ci.yaml))
   - Runs on pull requests to `main` and `release-*` branches
   - Performs linting (`go fmt`), static analysis (`go vet`), and build checks
   - Uses Go 1.22

2. **GoReleaser Workflow** ([`.github/workflows/goreleaser.yml`](.github/workflows/goreleaser.yml))
   - Triggers on version tags (e.g., `v1.0.0`)
   - Builds multi-architecture binaries (Linux/Darwin, amd64/arm64)
   - Creates and pushes Docker images to `ghcr.io/kagenti/kagenti-webhook`
   - Packages and publishes Helm charts to GHCR
   - Creates GitHub releases with changelog

3. **PR Verifier Workflow** ([`.github/workflows/pr-verifier.yaml`](.github/workflows/pr-verifier.yaml))
   - Validates PR titles follow semantic commit convention
   - Supports types: feat, fix, docs, style, refactor, perf, test, build, ci, chore, revert

4. **Spellcheck Workflow** ([`.github/workflows/spellcheck_action.yml`](.github/workflows/spellcheck_action.yml))
   - Runs on PRs and pushes to main
   - Checks spelling in Markdown files using custom wordlist

### Issue Templates

- **Bug Report** ([`.github/ISSUE_TEMPLATE/bug_report.yaml`](.github/ISSUE_TEMPLATE/bug_report.yaml))
- **Epic** ([`.github/ISSUE_TEMPLATE/epic.yaml`](.github/ISSUE_TEMPLATE/epic.yaml))
- **Feature Request** ([`.github/ISSUE_TEMPLATE/feature_request.yaml`](.github/ISSUE_TEMPLATE/feature_request.yaml))

### Spellcheck Configuration

- **Wordlist** ([`.github/spellcheck/.wordlist.txt`](.github/spellcheck/.wordlist.txt)) - 374 technical terms
- **Config** ([`.github/spellcheck/.spellcheck.yml`](.github/spellcheck/.spellcheck.yml)) - Spellcheck rules for Markdown and text files

### Other Configuration

- **Dependabot** ([`.github/dependabot.yaml`](.github/dependabot.yaml)) - Automated dependency updates for Go modules, Docker, and GitHub Actions
- **PR Template** ([`.github/pull_request_template.md`](.github/pull_request_template.md)) - Standardized PR description template

### Release Configuration

- **GoReleaser Config** ([`.goreleaser.yaml`](.goreleaser.yaml))
  - Multi-architecture builds for Linux and Darwin (amd64, arm64)
  - Docker image creation with multiple tags (version, major, minor, latest)
  - Multi-architecture Docker manifests
  - Automatic changelog generation

### Helm Chart Updates

- **Values** ([`charts/kagenti-webhook/values.yaml`](charts/kagenti-webhook/values.yaml))
  - Updated image repository to `ghcr.io/kagenti/kagenti-webhook`
  - Added version placeholder `__PLACEHOLDER__` for automated releases

- **New Templates**:
  - [`mutatingwebhook.yaml`](charts/kagenti-webhook/templates/mutatingwebhook.yaml) - Webhook configuration
  - [`validatingwebhook.yaml`](charts/kagenti-webhook/templates/validatingwebhook.yaml) - Validation webhook
  - [`clusterrole.yaml`](charts/kagenti-webhook/templates/clusterrole.yaml) - RBAC permissions
  - [`clusterrolebinding.yaml`](charts/kagenti-webhook/templates/clusterrolebinding.yaml) - Role binding
  - [`certificate-issuer.yaml`](charts/kagenti-webhook/templates/certificate-issuer.yaml) - Self-signed issuer
  - [`certificate.yaml`](charts/kagenti-webhook/templates/certificate.yaml) - TLS certificate

### Documentation

- **Deployment Guide** ([`DEPLOYMENT.md`](DEPLOYMENT.md)) - Comprehensive deployment instructions including:
  - CI/CD workflow documentation
  - Release creation process
  - Deployment methods (Helm, kubectl, Docker)
  - Troubleshooting guide
  - Local testing instructions

## How to Create a Release

```bash
# Tag and push
git tag -a v1.0.0 -m "Release v1.0.0"
git push origin v1.0.0
```

This automatically:
- Builds binaries for all platforms
- Creates Docker images for `ghcr.io/kagenti/kagenti-webhook:v1.0.0`
- Packages and publishes Helm chart to GHCR
- Creates GitHub release with artifacts and changelog

## Testing Checklist

- [ ] CI workflow runs successfully on PR
- [ ] PR verifier validates semantic commit titles
- [ ] Spellcheck passes on documentation
- [ ] GoReleaser dry-run succeeds: `goreleaser release --snapshot --clean`
- [ ] Helm chart templates correctly: `helm template kagenti-webhook ./charts/kagenti-webhook`
- [ ] Helm chart lints cleanly: `helm lint charts/kagenti-webhook`
- [ ] Webhook configurations are created
- [ ] Certificates are generated
- [ ] RBAC resources are applied

## Related Issues

Closes #XXX

## Additional Context

This implementation follows the same patterns as the `kagenti-operator` repository to ensure consistency across the Kagenti ecosystem.

## Dependencies

- **cert-manager** is required for webhook TLS certificates
- **GitHub Container Registry** permissions for publishing images and charts
