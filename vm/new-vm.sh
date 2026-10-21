#!/bin/sh
# Create a Lima VM that runs Shelley as a systemd service.
#
#   vm/new-vm.sh <name> [host-port]
#
# Shelley listens on port 9000 inside the VM; Lima forwards it to
# localhost:<host-port> on the Mac (the first free port from 9001 by default).
# Shelley gets its models from shelley-edge on the Mac, so no keys or
# sign-ins happen in the VM.
#
# Environment:
#   EDGE_URL       shelley-edge as seen from the VM (default http://host.lima.internal:8080)
#   SHELLEY_BIN    Linux Shelley binary (default bin/shelley-linux-<arch>, from make build-linux-*)
set -eu

usage() { echo "usage: $0 <name> [host-port]" >&2; exit 2; }
[ $# -ge 1 ] && [ $# -le 2 ] || usage
name=$1
case $name in
  *[!a-z0-9-]* | -* | "") echo "name must be lowercase letters, digits and dashes" >&2; exit 2 ;;
esac

repo=$(cd "$(dirname "$0")/.." && pwd)
edge_url=${EDGE_URL:-http://host.lima.internal:8080}
case $(uname -m) in
  arm64 | aarch64) bin_default=$repo/bin/shelley-linux-aarch64 make_target=build-linux-aarch64 ;;
  x86_64) bin_default=$repo/bin/shelley-linux-x86 make_target=build-linux-x86 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
shelley_bin=${SHELLEY_BIN:-$bin_default}
[ -x "$shelley_bin" ] || { echo "$shelley_bin not found; run: make $make_target" >&2; exit 1; }

if limactl list --quiet 2>/dev/null | grep -qx "$name"; then
  echo "a VM named $name already exists" >&2; exit 1
fi

# Ports promised to existing VMs, running or not, plus anything listening now.
port_taken() {
  grep -qs "hostPort: $1\$" "${LIMA_HOME:-$HOME/.lima}"/*/lima.yaml && return 0
  lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
}
if [ $# -eq 2 ]; then
  port=$2
  port_taken "$port" && { echo "port $port is already used" >&2; exit 1; }
else
  port=9001
  while port_taken "$port"; do port=$((port + 1)); done
fi

echo "==> creating VM $name (Shelley on localhost:$port)"
limactl create --tty=false --name "$name" \
  --set ".portForwards = [{\"guestPort\": 9000, \"hostPort\": $port}] + (.portForwards // [])" \
  "$repo/vm/shelley.yaml"
limactl start --tty=false "$name"

echo "==> installing Shelley"
limactl copy "$shelley_bin" "$name:/tmp/shelley"
limactl shell "$name" sudo install -m 0755 /tmp/shelley /usr/local/bin/shelley
limactl shell "$name" rm /tmp/shelley

guest_user=$(limactl shell "$name" id -un)
guest_home=$(limactl shell "$name" sh -c 'echo $HOME')

limactl shell "$name" mkdir -p .config/shelley .local/share/shelley
limactl shell "$name" sh -c 'cat > .config/shelley/shelley.json' <<EOF
{
  "llm_integrations": [
    {"name": "chatgpt", "url": "$edge_url/chatgpt"},
    {"name": "opencode", "url": "$edge_url/opencode"}
  ]
}
EOF

limactl shell "$name" sudo tee /etc/systemd/system/shelley.service >/dev/null <<EOF
[Unit]
Description=Shelley
After=network-online.target
Wants=network-online.target

[Service]
User=$guest_user
WorkingDirectory=$guest_home
Environment=HOME=$guest_home
ExecStart=/usr/local/bin/shelley -config $guest_home/.config/shelley/shelley.json -db $guest_home/.local/share/shelley/shelley.db serve -port 9000
Restart=on-failure

[Install]
WantedBy=multi-user.target
EOF
limactl shell "$name" sudo systemctl daemon-reload
limactl shell "$name" sudo systemctl enable --now shelley

echo "==> waiting for Shelley"
i=0
until curl -fs -o /dev/null "http://localhost:$port/"; do
  i=$((i + 1))
  [ $i -lt 60 ] || { echo "Shelley did not answer; see: limactl shell $name journalctl -u shelley" >&2; exit 1; }
  sleep 1
done
echo "Shelley is up at http://localhost:$port  (shell: limactl shell $name)"
