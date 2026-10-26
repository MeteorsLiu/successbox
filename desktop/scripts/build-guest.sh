#!/bin/bash
# Build on a native Linux arm64 runner. No Docker daemon or developer cache.
set -euo pipefail
output=$(realpath -m "$1")
test ! -e "$output"
mkdir -p "$output"
stage=$(mktemp -d)
trap 'if mountpoint -q "$stage/rootfs/proc"; then sudo umount "$stage/rootfs/proc" || exit 1; fi; sudo rm -rf "$stage"' EXIT
sudo debootstrap --variant=minbase bookworm "$stage/rootfs" https://deb.debian.org/debian
printf '#!/bin/sh\nexit 101\n' | sudo tee "$stage/rootfs/usr/sbin/policy-rc.d" >/dev/null
sudo chmod 755 "$stage/rootfs/usr/sbin/policy-rc.d"
sudo cp /etc/resolv.conf "$stage/rootfs/etc/resolv.conf"
sudo chroot "$stage/rootfs" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get update
sudo chroot "$stage/rootfs" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends xrdp xorgxrdp openbox xterm dbus-x11 dbus procps ca-certificates curl git python3 openssl slirp4netns socat iproute2 util-linux fonts-dejavu-core
curl --fail --location --retry 3 https://persistent.oaistatic.com/codex-app-prod/linux/deb/latest/chatgpt_arm64.deb -o "$stage/chatgpt.deb"
printf '68de1ec44066871e4177ba1ca793ebee6ba238397f13c794e45b14941c1261fb  %s\n' "$stage/chatgpt.deb" | sha256sum -c -
printf '68de1ec44066871e4177ba1ca793ebee6ba238397f13c794e45b14941c1261fb  chatgpt_arm64.deb\n' > "$output/chatgpt-download.sha256"
sudo cp "$stage/chatgpt.deb" "$stage/rootfs/tmp/chatgpt.deb"
sudo chroot "$stage/rootfs" /usr/bin/env DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends /tmp/chatgpt.deb
curl --fail --location --retry 3 https://nodejs.org/dist/v24.22.1/node-v24.22.1-linux-arm64.tar.xz -o "$stage/node-v24.22.1-linux-arm64.tar.xz"
curl --fail --location --retry 3 https://nodejs.org/dist/v24.22.1/SHASUMS256.txt -o "$stage/node-checksums.txt"
(cd "$stage" && grep ' node-v24.22.1-linux-arm64.tar.xz$' node-checksums.txt | sha256sum -c -)
sudo tar -xJf "$stage/node-v24.22.1-linux-arm64.tar.xz" --strip-components=1 -C "$stage/rootfs/usr/local"
sudo mount -t proc proc "$stage/rootfs/proc"
sudo chroot "$stage/rootfs" /usr/local/bin/npm install -g @anthropic-ai/claude-code@2.1.310
sudo umount "$stage/rootfs/proc"
sudo chroot "$stage/rootfs" dpkg-query -W -f='${Package} ${Version}\n' > "$output/packages.txt"
sudo chroot "$stage/rootfs" apt-get clean
sudo rm -rf "$stage/rootfs/root/.npm" "$stage/rootfs/root/.cache"
sudo rm -f "$stage/rootfs/tmp/chatgpt.deb" "$stage/rootfs/etc/machine-id"
sudo rm -rf "$stage/rootfs/var/lib/apt/lists/"*
sudo tar --exclude='rootfs/dev/*' --exclude='rootfs/proc/*' --exclude='rootfs/sys/*' -czf "$output/rootfs.tar.gz" -C "$stage" rootfs
# The Containerization init image is pinned by digest, independently of tags.
python3 - "$stage" <<'PY'
import hashlib,json,pathlib,sys,urllib.request
stage=pathlib.Path(sys.argv[1])
url='https://ghcr.io/token?service=ghcr.io&scope=repository:apple/containerization/vminit:pull'
token=json.load(urllib.request.urlopen(url))['token']
digest='609bfc0b6e66ff462364107c46830a918b975cf85080c6001c19291aa35efa25'
request=urllib.request.Request('https://ghcr.io/v2/apple/containerization/vminit/blobs/sha256:'+digest,headers={'Authorization':'Bearer '+token})
with urllib.request.urlopen(request,timeout=120) as source,(stage/'init.tar.gz').open('wb') as target:
 while block:=source.read(1024*1024):target.write(block)
with (stage/'init.tar.gz').open('rb') as f:
 assert hashlib.file_digest(f,'sha256').hexdigest()==digest,'init image checksum mismatch'
PY
mkdir "$stage/init"
sudo tar -xzf "$stage/init.tar.gz" -C "$stage/init"
truncate -s 512M "$output/initfs.ext4"
sudo mkfs.ext4 -F -q -d "$stage/init" "$output/initfs.ext4"
# Kata's 6.18.5 kernel enables the TUN driver required by slirp4netns.
curl --fail --location --retry 3 https://github.com/kata-containers/kata-containers/releases/download/3.26.0/kata-static-3.26.0-arm64.tar.zst -o "$stage/kata.tar.zst"
printf '83cf59d06ef66565338f78eb47c34a4885d8d79312178650efb5b2cdad357922  %s\n' "$stage/kata.tar.zst" | sha256sum -c -
mkdir "$stage/kata"
tar --zstd -xf "$stage/kata.tar.zst" -C "$stage/kata" ./opt/kata/share/kata-containers/config-6.18.5-177 ./opt/kata/share/kata-containers/vmlinux-6.18.5-177
grep -qx 'CONFIG_TUN=y' "$stage/kata/opt/kata/share/kata-containers/config-6.18.5-177"
cp "$stage/kata/opt/kata/share/kata-containers/vmlinux-6.18.5-177" "$output/vmlinux-arm64"
cp "$stage/kata/opt/kata/share/kata-containers/config-6.18.5-177" "$output/kernel.config"
sudo chown -R "$(id -u):$(id -g)" "$output"
