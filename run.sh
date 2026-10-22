#!/usr/bin/env bash
#
# run.sh — local build, test, and run wrapper for email-mcp.
#
# AWS deploy and image promotion are owned by MCPCICD
# (https://github.com/jpuckety/MCPCICD). This script is for laptop use
# (Go test/vet, local HTTP/stdio, docker, CDK synth) and for pushing this
# app's SSM secrets into Dev/Prod.
#
# Usage:
#   ./run.sh <command> [extra args...]
#
# Run "./run.sh help" for the full command list.
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="${SCRIPT_DIR}"
APP_DIR="${PROJECT_ROOT}/emailmcp"
WEB_DIR="${PROJECT_ROOT}/web"
CDK_DIR="${PROJECT_ROOT}/cdk"
ENV_FILE="${ENV_FILE:-${APP_DIR}/.env}"
CDK_JSON="${CDK_DIR}/cdk.json"
IMAGE_NAME="${IMAGE_NAME:-email-mcp:latest}"

log()  { printf '\033[1;34m[run]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[run]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[run]\033[0m %s\n' "$*" >&2; exit 1; }

require() {
  command -v "$1" >/dev/null 2>&1 || die "Required command '$1' is not installed or not on PATH."
}

load_dotenv() {
  local file="$1"
  [[ -f "${file}" ]] || return 0
  log "Loading environment variables from ${file#${PROJECT_ROOT}/}"
  local line key value
  while IFS= read -r line || [[ -n "${line}" ]]; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    [[ -z "${line}" || "${line}" == \#* ]] && continue
    line="${line#export }"
    [[ "${line}" == *=* ]] || continue
    key="${line%%=*}"
    value="${line#*=}"
    key="${key%"${key##*[![:space:]]}"}"
    key="${key#"${key%%[![:space:]]*}"}"
    [[ "${key}" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
    value="${value%"${value##*[![:space:]]}"}"
    value="${value#"${value%%[![:space:]]*}"}"
    if [[ ( "${value}" == \"*\" || "${value}" == \'*\' ) && ${#value} -ge 2 ]]; then
      value="${value:1:${#value}-2}"
    fi
    if [[ -z "${!key:-}" ]]; then
      export "${key}=${value}"
    fi
  done < "${file}"
}

relpath() {
  local path="$1"
  if [[ "$path" == "${PROJECT_ROOT}/"* ]]; then
    printf '%s' "${path#${PROJECT_ROOT}/}"
  else
    printf '%s' "$path"
  fi
}

with_profile() {
  local profile="${1:-}"
  shift
  if [[ -n "$profile" ]]; then
    AWS_PROFILE="$profile" "$@"
  else
    "$@"
  fi
}

ssm_parameter_name() {
  local key="$1"
  local fallback="$2"
  node -e '
    const fs = require("fs");
    let value = process.argv[3];
    try {
      const ssm = (JSON.parse(fs.readFileSync(process.argv[1], "utf8")).context || {}).ssm || {};
      if (ssm[process.argv[2]]) value = String(ssm[process.argv[2]]);
    } catch (err) {}
    process.stdout.write(value);
  ' "${CDK_JSON}" "$key" "$fallback"
}

put_secure_params() {
  while [[ $# -ge 2 ]]; do
    aws ssm put-parameter \
      --name "$1" \
      --type SecureString \
      --value "$2" \
      --overwrite \
      >/dev/null
    log "Pushed $1"
    shift 2
  done
}

go_in_app() {
  require go
  ( cd "${APP_DIR}" && go "$@" )
}

TEST_ISOLATED_VARS=(
  GOOGLE_CLIENT_ID GOOGLE_CLIENT_SECRET PUBLIC_BASE_URL APPLICATION_ID
  EMAILMCP_SESSION_TABLE EMAILMCP_USER_CONFIG_TABLE OAUTH_REDIRECT_ALLOWLIST
)

cmd_test() {
  require go
  cmd_build_web
  (
    cd "${APP_DIR}"
    local var
    for var in "${TEST_ISOLATED_VARS[@]}"; do
      unset "${var}"
    done
    go test ./... "$@"
  )
}

cmd_vet() {
  go_in_app vet ./... "$@"
}

cmd_build_web() {
  if command -v npm >/dev/null 2>&1 && [[ -d "${WEB_DIR}" ]]; then
    log "Building Angular frontend in ${WEB_DIR}..."
    ( cd "${WEB_DIR}" && [[ -d node_modules ]] || npm install --no-audit --no-fund )
    ( cd "${WEB_DIR}" && npm run build )
  else
    log "Skipping web build (npm not available or web/ missing)."
  fi
}

cmd_build() {
  cmd_build_web
  go_in_app build -o emailmcp ./cmd/emailmcp "$@"
}

cmd_check() {
  cmd_test "$@"
  cmd_vet
  cmd_build
}

cmd_clean() {
  rm -f "${APP_DIR}/emailmcp" "${APP_DIR}/emailmcp-x86_64"
  log "Removed local Go binaries."
}

cmd_run() {
  require go
  log "Starting HTTP MCP server from ${APP_DIR}..."
  ( cd "${APP_DIR}" && go run ./cmd/emailmcp "$@" )
}

cmd_run_stdio() {
  require go
  log "Starting stdio MCP server from ${APP_DIR}..."
  ( cd "${APP_DIR}" && go run ./cmd/emailmcp --transport stdio "$@" )
}

cmd_docker_build() {
  require docker
  log "Building container image '${IMAGE_NAME}' from Dockerfile..."
  ( cd "${PROJECT_ROOT}" && docker build -t "${IMAGE_NAME}" -f Dockerfile . "$@" )
  log "Built image: ${IMAGE_NAME}"
}

cdk_run() {
  require npx
  [[ -d "${CDK_DIR}/node_modules" ]] || cmd_cdk_install
  ( cd "${CDK_DIR}" && npx cdk "$@" )
}

cmd_cdk_install() {
  require npm
  log "Installing CDK dependencies..."
  ( cd "${CDK_DIR}" && npm install --no-audit --no-fund )
}

cmd_synth() {
  log "Synthesizing the CloudFormation template..."
  cdk_run synth "$@"
}

cmd_push_secrets() {
  local env_name="$1"
  local profile env_file client_id client_secret id_name secret_name account
  require aws
  require node

  case "$env_name" in
    Dev)
      profile="${DEV_PROFILE:-${AWS_PROFILE:-}}"
      env_file="${DEV_ENV_FILE:-${PROJECT_ROOT}/.env.dev}"
      ;;
    Prod)
      profile="${PROD_PROFILE:-${AWS_PROFILE:-}}"
      env_file="${PROD_ENV_FILE:-${PROJECT_ROOT}/.env.prod}"
      ;;
    *)
      die "unknown env '${env_name}'"
      ;;
  esac

  [[ -f "$env_file" ]] || die "missing $(relpath "$env_file"). Copy GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET there (emailmcp/.env is not pushed)."
  load_dotenv "$env_file"

  client_id="${GOOGLE_CLIENT_ID:-}"
  client_secret="${GOOGLE_CLIENT_SECRET:-}"
  [[ -n "$client_id" ]] || die "missing GOOGLE_CLIENT_ID in $(relpath "$env_file")"
  [[ -n "$client_secret" ]] || die "missing GOOGLE_CLIENT_SECRET in $(relpath "$env_file")"

  id_name="$(ssm_parameter_name googleClientId /email-mcp/google-client-id)"
  secret_name="$(ssm_parameter_name googleClientSecret /email-mcp/google-client-secret)"
  account="$(with_profile "$profile" aws sts get-caller-identity --query Account --output text)"
  log "Pushing SSM SecureString secrets to ${env_name} account ${account}"
  log "  Google OAuth from $(relpath "$env_file")"
  with_profile "$profile" put_secure_params \
    "$id_name" "$client_id" \
    "$secret_name" "$client_secret"
  log "Done. EmailMcpStack can now read ${id_name} and ${secret_name}."
}

usage() {
  cat <<'EOF'
run.sh — local build, test, and run wrapper for email-mcp

Usage: ./run.sh <command> [extra args...]

AWS deploy and image promotion live in MCPCICD. SSM secrets for this app
are pushed from here.

Build & test:
  test             go test ./... in emailmcp/ (runtime secrets unset).
  vet              go vet ./...
  build            Build the native binary in emailmcp/.
  check            test + vet + build.
  clean            Remove local Go binaries.

Run locally:
  run              Run the HTTP MCP server on :8080.
  run-stdio        Run the stdio MCP server (logs to stderr).

Container:
  docker-build     Build the container image from the repo-root Dockerfile
                   (IMAGE_NAME env, default email-mcp:latest).

CDK (local check only):
  cdk-install      Install CDK Node dependencies (cdk/).
  synth            Synthesize the CloudFormation template.

SSM secrets (this app, current AWS account):
  push-secrets-dev Write /email-mcp/google-client-id and google-client-secret
                   from .env.dev (emailmcp/.env is not used).
  push-secrets-prod Same for Prod from .env.prod.

Environment overrides:
  IMAGE_NAME        Docker image tag for docker-build (default email-mcp:latest).
  ENV_FILE          Path to the laptop KEY=VALUE file (default: emailmcp/.env).
  DEV_PROFILE / PROD_PROFILE   AWS CLI profile for push-secrets-* (else AWS_PROFILE).
  DEV_ENV_FILE / PROD_ENV_FILE KEY=VALUE files (default: .env.dev / .env.prod).

Configuration file:
  emailmcp/.env     Optional, git-ignored KEY=VALUE file loaded automatically
                    on local commands. Values already set in the environment
                    take precedence. Pipeline / ECS do not read this file.
  .env.dev / .env.prod
                    Git-ignored files for ./run.sh push-secrets-dev|prod.

Any extra args after the command are forwarded to the underlying tool.
EOF
}

main() {
  [[ -d "${APP_DIR}" && -f "${APP_DIR}/go.mod" ]] || die "This script must be run from the EmailMCP project root."

  local cmd="${1:-help}"
  [[ $# -gt 0 ]] && shift || true

  case "${cmd}" in
    push-secrets-dev|push_secrets_dev)
      cmd_push_secrets Dev "$@"
      return
      ;;
    push-secrets-prod|push_secrets_prod)
      cmd_push_secrets Prod "$@"
      return
      ;;
    push-secrets|push_secrets)
      die "use ./run.sh push-secrets-dev or ./run.sh push-secrets-prod (emailmcp/.env is not pushed)"
      ;;
  esac

  load_dotenv "${ENV_FILE}"

  case "${cmd}" in
    test)                          cmd_test "$@" ;;
    vet)                           cmd_vet "$@" ;;
    build)                         cmd_build "$@" ;;
    build-web|build_web)           cmd_build_web "$@" ;;
    check)                         cmd_check "$@" ;;
    clean)                         cmd_clean "$@" ;;
    run|run-http|run_http)         cmd_run "$@" ;;
    run-stdio|run_stdio)           cmd_run_stdio "$@" ;;
    docker-build|docker_build)     cmd_docker_build "$@" ;;
    cdk-install|cdk_install)       cmd_cdk_install "$@" ;;
    synth)                         cmd_synth "$@" ;;
    help|-h|--help)                usage ;;
    eks-deploy|undeploy-eks|eks-refresh|eks-status|eks-logs|infra-deploy|infra-destroy|build-push|deploy|undeploy|destroy)
      die "'${cmd}' was removed from this repo. Use MCPCICD for AWS bootstrap and deploy."
      ;;
    *) warn "Unknown command: ${cmd}"; echo; usage; exit 2 ;;
  esac
}

main "$@"
