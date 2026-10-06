#!/usr/bin/env bash
set -euo pipefail

test_dir=$(mktemp -d)
test_name="filemind-validation-$$"
test_image="$test_name:local"
cleanup() {
  docker rm -f "$test_name" >/dev/null 2>&1 || true
  docker volume rm "$test_name" >/dev/null 2>&1 || true
  docker image rm "$test_image" >/dev/null 2>&1 || true
  rm -rf "$test_dir"
}
trap cleanup EXIT
trap 'printf "Container smoke test failed at line %s (exit %s).\n" "$LINENO" "$?" >&2' ERR
docker build -t "$test_image" .
docker run -d --name "$test_name" --read-only --cap-drop ALL \
  --security-opt no-new-privileges:true --memory 512m --pids-limit 128 \
  --tmpfs /tmp:size=16m,mode=1777,noexec,nosuid,nodev \
  -v "$test_name:/data" \
  -p 127.0.0.1::8080 -p 127.0.0.1::8081 -p 127.0.0.1::8082 \
  -e FILEMIND_OWNER_URL=https://owner.example.test \
  -e FILEMIND_PUBLIC_URL=https://public.example.test \
  -e FILEMIND_ADMIN_URL=https://admin.example.test \
  -e FILEMIND_ADMIN_LISTEN=0.0.0.0:8082 "$test_image" >/dev/null
ready=false
for attempt in {1..40}; do
  if docker exec "$test_name" /filemind healthcheck >/dev/null 2>&1; then ready=true; break; fi
  sleep 0.25
done
if [[ "$ready" != true ]]; then docker logs "$test_name"; exit 1; fi
owner="http://$(docker port "$test_name" 8080/tcp)"
public="http://$(docker port "$test_name" 8081/tcp)"
admin="http://$(docker port "$test_name" 8082/tcp)"
curl --fail --silent "$owner/login" > "$test_dir/login"
curl --fail --silent "$owner/assets/hash-worker.js" > "$test_dir/worker"
test -s "$test_dir/worker"
test "$(curl --silent -o /dev/null -w '%{http_code}' "$owner/admin/users")" = 404
test "$(curl --silent -o /dev/null -w '%{http_code}' "$public/upload")" = 404
test "$(curl --silent -o /dev/null -w '%{http_code}' "$admin/login")" = 303
test "$(curl --silent -o /dev/null -w '%{http_code}' "$owner/setup")" = 404
test "$(curl --silent -o /dev/null -w '%{http_code}' "$public/setup")" = 404
curl --fail --silent "$admin/setup" > "$test_dir/setup"
setup_cookie=$(python3 - "$test_dir/setup" <<'PY'
import hashlib
import re
import sys
from pathlib import Path
token = re.search(r'name="csrf-token" content="([^"]+)"', Path(sys.argv[1]).read_text()).group(1)
name = '__Secure-filemind-csrf-' + hashlib.sha256(b'admin-setup').hexdigest()[:16]
print(name + '=' + token)
PY
)
curl --fail --silent "$admin/setup" \
  -H 'Origin: https://admin.example.test' -H 'Content-Type: application/json' \
  -H "Cookie: $setup_cookie" -H "X-CSRF-Token: ${setup_cookie#*=}" \
  --data '{"username":"container-admin","password":"container-password-for-validation"}' > /dev/null
test "$(curl --silent -o /dev/null -w '%{http_code}' "$admin/setup")" = 303
test "$(curl --silent -X POST -o /dev/null -w '%{http_code}' "$admin/setup")" = 404
docker restart "$test_name" >/dev/null
admin="http://$(docker port "$test_name" 8082/tcp)"
for attempt in {1..40}; do
  if docker exec "$test_name" /filemind healthcheck >/dev/null 2>&1; then
    test "$(curl --silent -o /dev/null -w '%{http_code}' "$admin/setup")" = 303
    curl --fail --silent "$admin/login" > /dev/null
    exit 0
  fi
  sleep 0.25
done
docker logs "$test_name"
exit 1
