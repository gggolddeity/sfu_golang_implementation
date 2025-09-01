#!/usr/bin/env bash
set -Eeuo pipefail

APP_NAME="webrtc_calls"
GO_MAIN="./cmd/app"                 # путь к main (dir или файл)
GOOS_TARGET="linux"
GOARCH_TARGET="amd64"
CGO_ENABLED=1

STATIC_LOCAL_DIR="${STATIC_LOCAL_DIR:-frontend}"

HOST="79.137.199.239"
SSH_USER="deploy"
SSH_KEY="${SSH_KEY:-/home/deity/.ssh/id_rsa}"
PORT=22

# Пути на сервере (боевые)
APP_DIR="/opt/${APP_NAME}"
BIN_DIR="${APP_DIR}/bin"
CONFIG_DIR="${APP_DIR}/config"
STATIC_REMOTE_DIR="/opt/${APP_NAME}/${STATIC_LOCAL_DIR}/"
LOG_DIR="/var/log/${APP_NAME}"

APP_USER="${SSH_USER}"
SERVICE_NAME="${APP_NAME}.service"

ENV_FILE="${ENV_FILE:-}"

SERVER_NAME="aigism.ru"
ACME_WEBROOT="/var/www/letsencrypt"
CERT_FULLCHAIN="/etc/letsencrypt/live/${SERVER_NAME}/fullchain.pem"
CERT_PRIVKEY="/etc/letsencrypt/live/${SERVER_NAME}/privkey.pem"
API_PORT="8081"

SSH_OPTS=(-o "Port=${PORT}" -i "${SSH_KEY}" -o "StrictHostKeyChecking=accept-new")
DEST="${SSH_USER}@${HOST}"

ssh_do() { ssh "${SSH_OPTS[@]}" "${DEST}" "$@"; }
scp_put() {
  scp "${SSH_OPTS[@]}" "$1" "${DEST}:$2"
}
RSYNC_RSH="ssh -o Port=${PORT} -i ${SSH_KEY} -o StrictHostKeyChecking=accept-new"

remote_sudo() {
  if [[ -n "${SUDO_PASS:-}" ]]; then
    ( printf '%s\n' "$SUDO_PASS"; cat - ) | ssh "${SSH_OPTS[@]}" "${DEST}" 'sudo -S bash -s'
  else
    ssh "${SSH_OPTS[@]}" "${DEST}" 'sudo -n bash -s' || {
      echo "sudo требует пароль. Запусти с SUDO_PASS='***'"; exit 1; }
  fi
}

build_locally() {
  echo "==> Go build"
  mkdir -p build
  CGO_ENABLED=${CGO_ENABLED} GOOS="${GOOS_TARGET}" GOARCH="${GOARCH_TARGET}" \
    go build -trimpath -ldflags "-s -w" -o "build/${APP_NAME}" "${GO_MAIN}"

  echo "==> Проверка статики"
  [[ -d "${STATIC_LOCAL_DIR}" ]] || { echo "Нет каталога ${STATIC_LOCAL_DIR}"; exit 1; }
  [[ -f "${STATIC_LOCAL_DIR}/index.html" ]] || { echo "Нет ${STATIC_LOCAL_DIR}/index.html"; exit 1; }
}

init_server() {
  echo "==> Init server (пакеты, директории, systemd, nginx)"
  remote_sudo <<'REMOTE_CMDS'
set -euo pipefail

APP_NAME="webrtc_calls"
APP_USER="deploy"
APP_DIR="/opt/${APP_NAME}"
BIN_DIR="${APP_DIR}/bin"
CONFIG_DIR="${APP_DIR}/config"
LOG_DIR="/var/log/${APP_NAME}"
STATIC_REMOTE_DIR="/var/www/${APP_NAME}"
ACME_WEBROOT="/var/www/letsencrypt"
API_PORT="8081"
SERVER_NAME="aigism.ru"
CERT_FULLCHAIN="/etc/letsencrypt/live/${SERVER_NAME}/fullchain.pem"
CERT_PRIVKEY="/etc/letsencrypt/live/${SERVER_NAME}/privkey.pem"

if command -v apt-get >/dev/null 2>&1; then
  apt-get update -y
  # рантайм + nginx + инструменты
  apt-get install -y nginx rsync curl jq pkg-config \
    libopus0 libogg0 libopusfile0
  ldconfig
else
  echo "Нужен Ubuntu/Debian с apt-get"; exit 1
fi

# завести пользователя при необходимости (если уже есть — пропустит)
id -u "${APP_USER}" >/dev/null 2>&1 || useradd -m -s /bin/bash "${APP_USER}" || true

mkdir -p "${BIN_DIR}" "${CONFIG_DIR}" "${LOG_DIR}" "${STATIC_REMOTE_DIR}" "${ACME_WEBROOT}"
chown -R ${APP_USER}:${APP_USER} "${APP_DIR}" "${LOG_DIR}" "${STATIC_REMOTE_DIR}"

touch /opt/webrtc_calls/config/env

# systemd unit (ENV-файл опциональный с префиксом "-")
cat >/etc/systemd/system/${APP_NAME}.service <<UNIT
[Unit]
Description=${APP_NAME} service
After=network.target

[Service]
User=${APP_USER}
Group=${APP_USER}
WorkingDirectory=${APP_DIR}
Environment=API_PORT=${API_PORT}
EnvironmentFile=-${CONFIG_DIR}/env
ExecStart=${BIN_DIR}/${APP_NAME}
Restart=on-failure
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT

# NGINX: всегда HTTP; HTTPS добавляем только если есть сертификаты
CONF_AVAIL=" /etc/nginx/conf.d/${APP_NAME}.conf"
CONF_ENABLED="/etc/nginx/sites-enabled/${APP_NAME}.conf"
rm -f "$CONF_ENABLED" "$CONF_AVAIL" 2>/dev/null || true

cat >"$CONF_AVAIL" <<HTTP80
server {
    listen 80;
    server_name ${SERVER_NAME};

     location ^~ /.well-known/acme-challenge/ {
          root /var/www/letsencrypt;
          default_type "text/plain";
    }

    location / {
          return 301 https://$host$request_uri;
    }
}
HTTP80

if [ -f "${CERT_FULLCHAIN}" ] && [ -f "${CERT_PRIVKEY}" ]; then
cat >>"$CONF_AVAIL" <<HTTPS443
server {
    listen 443 ssl http2;
    server_name ${SERVER_NAME};

    ssl_certificate     /etc/letsencrypt/live/aigism.ru/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/aigism.ru/privkey.pem;

    # --- Вебсокеты к Go на :8081 ---
    # Совпадает и для /subscribe и для /subscribe/...
    location ^~ /subscribe {
       proxy_pass http://127.0.0.1:8081;      # твой Go-WS
       proxy_http_version 1.1;
       proxy_set_header Host              $host;
       proxy_set_header X-Real-IP         $remote_addr;
       proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
       proxy_set_header X-Forwarded-Proto $scheme;

       # ключевые хедеры для WS:
       proxy_set_header Upgrade           $http_upgrade;
       proxy_set_header Connection        "upgrade";

       proxy_read_timeout  1h;
       proxy_send_timeout  1h;
       proxy_buffering     off;
    }

     location / {
            proxy_pass http://127.0.0.1:8081;
            proxy_http_version 1.1;
            proxy_set_header Host              $host;
            proxy_set_header X-Real-IP         $remote_addr;
            proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
            proxy_set_header X-Forwarded-Proto $scheme;
            proxy_read_timeout 60s;
     }
}
HTTPS443
fi

ln -sf "$CONF_AVAIL" "$CONF_ENABLED"
rm -f /etc/nginx/sites-enabled/default || true

nginx -t
systemctl daemon-reload
systemctl enable ${APP_NAME}.service || true
systemctl restart nginx
REMOTE_CMDS
  echo "==> init завершён."
}

deploy_remote() {
  set -Eeuo pipefail

  local build_path="build/${APP_NAME}"
  [[ -f "${build_path}" ]] || { echo "Нет ${build_path}. Сначала: $0 build"; exit 1; }

  # staging не зависит от $HOME (на некоторых серверах его может не быть)
  local REMOTE_STAGE="/tmp/.deploy_${APP_NAME}"
  local REMOTE_STATIC_STAGE="${REMOTE_STAGE}/static"

  echo "==> Создаю stage на удалёнке: ${REMOTE_STAGE}"
  ssh_do "mkdir -p ${REMOTE_STATIC_STAGE} && ls -ld ${REMOTE_STAGE} ${REMOTE_STATIC_STAGE}"

  echo "==> Копирую бинарь"
  scp_put "${build_path}" "${REMOTE_STAGE}/${APP_NAME}.new"
  ssh_do "test -f ${REMOTE_STAGE}/${APP_NAME}.new && ls -l ${REMOTE_STAGE}/${APP_NAME}.new"

  if [[ -n "${ENV_FILE:-}" && -f "${ENV_FILE}" ]]; then
    echo "==> Копирую env (${ENV_FILE})"
    scp_put "${ENV_FILE}" "${REMOTE_STAGE}/${APP_NAME}.env"
    ssh_do "ls -l ${REMOTE_STAGE}/${APP_NAME}.env"
  else
    echo "==> ENV не задан — пропускаю."
  fi

  echo "==> Синхронизирую статику в stage"
  rsync -az --delete -e "${RSYNC_RSH}" "${STATIC_LOCAL_DIR}/" "${DEST}:${REMOTE_STATIC_STAGE}/"
  ssh_do "ls -l ${REMOTE_STATIC_STAGE} | head -n 50 || true"

  echo "==> Установка под sudo"
  remote_sudo <<REMOTE_CMDS
set -euo pipefail
APP_NAME="${APP_NAME}"
APP_USER="${APP_USER}"
BIN_DIR="${BIN_DIR}"
CONFIG_DIR="${CONFIG_DIR}"
STATIC_REMOTE_DIR="${STATIC_REMOTE_DIR}"
REMOTE_STAGE="/tmp/.deploy_${APP_NAME}"
REMOTE_STATIC_STAGE="\${REMOTE_STAGE}/static"

mkdir -p "\${BIN_DIR}" "\${CONFIG_DIR}" "\${STATIC_REMOTE_DIR}"

# бинарь
install -m 0755 "\${REMOTE_STAGE}/\${APP_NAME}.new" "\${BIN_DIR}/\${APP_NAME}"
chown "\${APP_USER}:\${APP_USER}" "\${BIN_DIR}/\${APP_NAME}"
rm -f "\${REMOTE_STAGE}/\${APP_NAME}.new" || true

# env (если есть)
if [ -f "\${REMOTE_STAGE}/\${APP_NAME}.env" ]; then
  install -m 0640 "\${REMOTE_STAGE}/\${APP_NAME}.env" "\${CONFIG_DIR}/env"
  chown "\${APP_USER}:\${APP_USER}" "\${CONFIG_DIR}/env"
  rm -f "\${REMOTE_STAGE}/\${APP_NAME}.env" || true
fi

# статика
rsync -a --delete "\${REMOTE_STATIC_STAGE}/" "\${STATIC_REMOTE_DIR}/"
chown -R "\${APP_USER}:\${APP_USER}" "\${STATIC_REMOTE_DIR}"
rm -rf "\${REMOTE_STATIC_STAGE}" || true
REMOTE_CMDS

  echo "==> Перезапуск сервиса и nginx"
  remote_sudo <<REMOTE_CMDS
set -euo pipefail
systemctl daemon-reload
systemctl restart "${SERVICE_NAME}" || true
sleep 1
systemctl --no-pager -l status "${SERVICE_NAME}" | sed -n '1,160p' || true
nginx -t && systemctl reload nginx
REMOTE_CMDS

  echo "==> Deploy завершён."
}

open_ssh() {
  exec ssh "${SSH_OPTS[@]}" "${DEST}"
}

usage() {
  cat <<EOF
Usage:
  SUDO_PASS='...' $0 init
  $0 build
  SUDO_PASS='...' $0 deploy
  $0 ssh
EOF
}

### ---------- ENTRY ----------
case "${1:-}" in
  init)   init_server ;;
  build)  build_locally ;;
  deploy) build_locally; deploy_remote ;;
  ssh)    open_ssh ;;
  *)      usage; exit 1 ;;
esac
