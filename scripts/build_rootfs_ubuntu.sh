#!/bin/bash
# 构建最小可运行 rootfs（Ubuntu 22.04 / 类 Debian，multiarch 布局）
# 目标：/opt/runner/rootfs/base
#
# 特点（相对 RHEL 版）：
#   - 库目录为 multiarch：/lib/x86_64-linux-gnu、/usr/lib/x86_64-linux-gnu
#   - Python 3.10（libpython3.10.so.1.0）
#   - gcc / gcc-11；vim 经 alternatives 指向 /usr/bin/vim.basic
#   - locale 为归档文件 /usr/lib/locale/locale-archive
#   - NSS(sssd) 模块位于 multiarch 库目录
#   - vim 依赖 libsodium.so.23 / libgpm.so.2
#
# 从宿主拷贝二进制 + ldd 递归依赖；并补入 NSS(sssd)、locale、用户/组、去 setuid。
# 必须在目标服务器（Ubuntu）上以 root 运行。
# 目标目录可用环境变量覆盖：ROOT=/tmp/xx bash build_rootfs_ubuntu.sh
set -euo pipefail

ROOT="${ROOT:-/opt/runner/rootfs/base}"

echo ">>> 清空并重建目录结构"
rm -rf "$ROOT"
mkdir -p "$ROOT"/{bin,sbin,usr/bin,usr/sbin,usr/lib,usr/lib64,usr/lib/x86_64-linux-gnu,lib,lib64,lib/x86_64-linux-gnu,etc,home,tmp,proc,dev,dev/pts,dev/shm,root,var/tmp,opt}
chmod 1777 "$ROOT"/tmp "$ROOT"/var/tmp
chmod 1777 "$ROOT"/dev/shm

# 需拷贝的二进制（核心工具 + 编译运行工具）
BINS=(
  /bin/bash /bin/sh /usr/bin/bash
  /usr/bin/ls /usr/bin/cat /usr/bin/grep /usr/bin/awk /usr/bin/sed
  /usr/bin/mkdir /usr/bin/cp /usr/bin/rm /usr/bin/mv /usr/bin/ln
  /usr/bin/touch /usr/bin/pwd /usr/bin/whoami /usr/bin/id /usr/bin/uname
  /usr/bin/ps /usr/bin/top /usr/bin/env /usr/bin/find /usr/bin/xargs
  /usr/bin/head /usr/bin/tail /usr/bin/wc /usr/bin/sort /usr/bin/uniq
  /usr/bin/cut /usr/bin/tr /usr/bin/tee /usr/bin/diff /usr/bin/patch
  /usr/bin/make /usr/bin/gcc /usr/bin/gcc-11 /usr/bin/cc /usr/bin/as /usr/bin/ld
  /usr/bin/python3 /usr/bin/python3.10 /usr/bin/perl
  /usr/bin/readlink /usr/bin/realpath /usr/bin/basename /usr/bin/dirname
  /usr/bin/su /usr/bin/login /usr/bin/chown /usr/bin/chmod /usr/bin/chgrp
  /usr/bin/kill /usr/bin/killall /usr/bin/which /usr/bin/uptime
  /usr/bin/df /usr/bin/du /usr/bin/stat /usr/bin/date
  /usr/bin/gzip /usr/bin/gunzip /usr/bin/tar /usr/bin/bzip2 /usr/bin/xz
  /usr/bin/curl /usr/bin/wget /usr/bin/ping
)

copy_bin() {
  local src="$1" name
  name="$(basename "$src")"
  if [ ! -e "$src" ]; then
    echo "  [skip] $src (不存在)"
    return 0
  fi
  # 符号链接：保持链接关系（目标可能已在列表）
  if [ -L "$src" ]; then
    local target
    target="$(readlink "$src")"
    local dest="/usr/bin/$name"
    # 尽量放到目标对应的系统路径
    case "$src" in /bin/*|/sbin/*) dest="/bin/$name";; esac
    ln -sfn "$target" "$ROOT$dest" 2>/dev/null || true
    # 拷贝链接目标（若目标不在 BINS 中）
    local realsrc
    realsrc="$(readlink -f "$src")"
    if [ "$realsrc" != "$src" ]; then
      copy_dep "$realsrc"
    fi
    return 0
  fi
  # 普通文件：放到对应系统路径
  local dest
  case "$src" in
    /bin/*|/sbin/*) dest="/bin/$name";;
    /usr/bin/*|/usr/sbin/*) dest="/usr/bin/$name";;
    *) dest="/usr/bin/$name";;
  esac
  if [ ! -e "$ROOT$dest" ]; then
    cp -a "$src" "$ROOT$dest" 2>/dev/null || { echo "  [fail] copy $src"; return 0; }
    echo "  [bin] $src -> $dest"
  fi
  copy_dep "$src"
}

# 递归拷贝动态库依赖（保持宿主的绝对路径结构，使符号链接可解析）
declare -A SEEN
copy_dep() {
  local src="$1" line lib libname
  if ! command -v ldd >/dev/null 2>&1; then return; fi
  while IFS= read -r line; do
    case "$line" in
      *'=> /'*)
        lib="${line#*=> }"
        lib="${lib%% *}"
        ;;
      *'/lib/'*|*'/lib64/'*|*'/usr/lib/'*)
        lib="$(echo "$line" | grep -oE '/[^ ]+\.so[^ ]*' | head -1 || true)"
        ;;
      *) continue;;
    esac
    [ -z "$lib" ] && continue
    [ -e "$lib" ] || continue
    libname="$(basename "$lib")"
    # 用绝对路径去重（basename 会导致符号链接与其真实目标同名被误去重）
    if [ -n "${SEEN[$lib]:-}" ]; then continue; fi
    SEEN["$lib"]=1
    # 目标路径：保持与宿主相同的绝对路径（去前导 /），符号链接才能解析
    local dest="${lib#/}"
    local destfull="$ROOT/$dest"
    mkdir -p "$(dirname "$destfull")"
    if [ -L "$lib" ]; then
      # 保持符号链接本身及其绝对路径位置
      [ -e "$destfull" ] || cp -aP "$lib" "$destfull" 2>/dev/null || true
      local real
      real="$(readlink -f "$lib")"
      [ -n "$real" ] || return 0
      # 关键修复：把真实目标文件本身拷入 rootfs
      # 1) 符号链接所在目录（相对链接可解析，如 /lib64/libtinfo.so.6.2）
      local linkdir
      linkdir="$(dirname "$lib")"
      mkdir -p "$ROOT/${linkdir#/}"
      [ -e "$ROOT/${linkdir#/}/$(basename "$real")" ] || cp -a "$real" "$ROOT/${linkdir#/}/$(basename "$real")" 2>/dev/null || true
      # 2) 宿主绝对路径位置（兼容绝对链接，如 /usr/lib64/libtinfo.so.6.2）
      mkdir -p "$ROOT/$(dirname "${real#/}")"
      [ -e "$ROOT/${real#/}" ] || cp -a "$real" "$ROOT/${real#/}" 2>/dev/null || true
      if [ -n "${SEEN[$real]:-}" ]; then continue; fi
      copy_dep "$real"
    else
      [ -e "$destfull" ] || cp -a "$lib" "$destfull" 2>/dev/null || true
    fi
  done < <(ldd "$src" 2>/dev/null)
}

echo ">>> 拷贝二进制与依赖"
for b in "${BINS[@]}"; do
  copy_bin "$b"
done

echo ">>> 拷贝常用动态库（libgcc_s, libstdc++, libpython, libcrypto, libssl, zlib）"
# 候选库目录（Ubuntu 与 RHEL 通用）
LIB_DIRS=(/lib/x86_64-linux-gnu /usr/lib/x86_64-linux-gnu /lib64 /usr/lib64)
# 常用库（按文件名）
COMMON_LIBS=(libgcc_s.so.1 libstdc++.so.6 libpython3.10.so.1.0 libpython3.9.so.1.0 libcrypto.so.3 libssl.so.3 libz.so.1 libffi.so.8 libexpat.so.1 libmpc.so.3 libmpfr.so.6 libgmp.so.10 libisl.so.23 libzstd.so.1 libbz2.so.1.0 liblzma.so.5 libreadline.so.8 libtinfo.so.6 libsqlite3.so.0 libuuid.so.1 libpcre2-8.so.0 libpthread.so.0 libdl.so.2 librt.so.1)
for name in "${COMMON_LIBS[@]}"; do
  for d in "${LIB_DIRS[@]}"; do
    [ -e "$d/$name" ] || continue
    # 符号链接：拷链接 + 真实文件；普通文件：直接拷；同时 ldd 递归依赖
    if [ -L "$d/$name" ]; then
      real="$(readlink -f "$d/$name")"
      [ -e "$ROOT/${d#/}/$name" ] || cp -aP "$d/$name" "$ROOT/${d#/}/$name" 2>/dev/null || true
      mkdir -p "$ROOT/${d#/}"
      [ -e "$ROOT/${d#/}/$(basename "$real")" ] || cp -a "$real" "$ROOT/${d#/}/$(basename "$real")" 2>/dev/null || true
      mkdir -p "$ROOT/$(dirname "${real#/}")"
      [ -e "$ROOT/${real#/}" ] || cp -a "$real" "$ROOT/${real#/}" 2>/dev/null || true
      copy_dep "$d/$name"
    else
      [ -e "$ROOT/${d#/}/$name" ] || cp -a "$d/$name" "$ROOT/${d#/}/$name" 2>/dev/null || true
      copy_dep "$d/$name"
    fi
    break
  done
done

echo ">>> 拷贝 NSS 模块（libnss_sss / libnss_systemd）及其动态依赖"
# AD/LDAP 用户经 sssd（nsswitch: passwd: files systemd sss）解析，rootfs 需含
# libnss_sss.so.2（及 libnss_systemd.so.2）才能按名解析 AD 用户/组；配合
# sandbox.bind_mounts 把 /var/lib/sss/pipes 与 /var/lib/sss/mc 映射进沙箱。
for mod in libnss_sss.so.2 libnss_systemd.so.2; do
  for d in /lib/x86_64-linux-gnu /usr/lib/x86_64-linux-gnu; do
    [ -e "$d/$mod" ] || continue
    mkdir -p "$ROOT/${d#/}"
    [ -e "$ROOT/${d#/}/$mod" ] || cp -a "$d/$mod" "$ROOT/${d#/}/$mod" 2>/dev/null || true
    copy_dep "$d/$mod"
    break
  done
done

echo ">>> 创建必要符号链接"
ln -sfn /usr/bin/python3 "$ROOT/usr/bin/python" 2>/dev/null || true
ln -sfn /bin/bash "$ROOT/bin/sh" 2>/dev/null || true
# /lib64/ld-linux-x86-64.so.2 必须存在（若上面没拷到）
if [ ! -e "$ROOT/lib64/ld-linux-x86-64.so.2" ] && [ -e /lib64/ld-linux-x86-64.so.2 ]; then
  cp -aP /lib64/ld-linux-x86-64.so.2 "$ROOT/lib64/" 2>/dev/null || true
fi
# 强制拷贝真实链接器文件（符号链接真实目标，不能被去重跳过）
for cand in /lib/x86_64-linux-gnu/ld-linux-x86-64.so.2 /usr/lib/x86_64-linux-gnu/ld-linux-x86-64.so.2; do
  if [ -e "$cand" ] && [ ! -e "$ROOT/${cand#/}" ]; then
    mkdir -p "$(dirname "$ROOT/${cand#/}")"
    cp -a "$cand" "$ROOT/${cand#/}" 2>/dev/null || true
  fi
done
# /lib/x86_64-linux-gnu 是 libc 等库的宿主路径，确保存在
mkdir -p "$ROOT/lib/x86_64-linux-gnu" "$ROOT/usr/lib/x86_64-linux-gnu"

echo ">>> 写入/拷贝 /etc 文件"
# 用户/组：优先拷贝宿主 /etc/passwd /etc/group /etc/nsswitch.conf，
# 使沙箱内 id / 提示符能解析真实用户名与补充组（本地/已 provision 用户）。
if [ -e /etc/passwd ]; then
  cp -a /etc/passwd "$ROOT/etc/passwd"
else
  cat > "$ROOT/etc/passwd" <<'EOF'
root:x:0:0:root:/root:/bin/bash
daemon:x:1:1:daemon:/usr/sbin:/usr/sbin/nologin
nobody:x:65534:65534:nobody:/nonexistent:/usr/sbin/nologin
EOF
fi
if [ -e /etc/group ]; then
  cp -a /etc/group "$ROOT/etc/group"
else
  cat > "$ROOT/etc/group" <<'EOF'
root:x:0:
daemon:x:1:
nobody:x:65534:
EOF
fi
[ -e /etc/nsswitch.conf ] && cp -a /etc/nsswitch.conf "$ROOT/etc/nsswitch.conf" 2>/dev/null || true
: > "$ROOT/etc/mtab"
: > "$ROOT/etc/fstab"
echo "127.0.0.1 localhost" > "$ROOT/etc/hosts"
echo "nameserver 8.8.8.8" > "$ROOT/etc/resolv.conf" 2>/dev/null || true
printf 'root:x:0:0:root:/root:/bin/bash\n' > "$ROOT/etc/shadow" 2>/dev/null || true

echo ">>> 拷贝 locale（消除 setlocale 警告）"
mkdir -p "$ROOT/usr/lib/locale"
[ -e /usr/lib/locale/locale-archive ] && cp -a /usr/lib/locale/locale-archive "$ROOT/usr/lib/locale/locale-archive" 2>/dev/null || true

echo ">>> 去除 setuid/setgid 位（安全加固）"
find "$ROOT" -perm -4000 -type f -exec chmod -s {} + 2>/dev/null || true
find "$ROOT" -perm -2000 -type f -exec chmod -s {} + 2>/dev/null || true

echo ">>> 完成"
du -sh "$ROOT"
echo ">>> bash 是否可运行（chroot 测试，需 root）"
chroot "$ROOT" /bin/bash -c 'echo OK bash works; /usr/bin/id; /usr/bin/ls /' 2>&1 | head -20

# === vim/vi 编辑器补充 ===
# 补充 libsodium/libgpm 依赖（vim 需要，位于 /lib/x86_64-linux-gnu）
for lib in libsodium.so.23.3.0 libgpm.so.2; do
  for d in /usr/lib/x86_64-linux-gnu /lib/x86_64-linux-gnu; do
    [ -e "$d/$lib" ] && cp -a "$d/$lib" "$ROOT/lib/x86_64-linux-gnu/" 2>/dev/null
  done
done
ln -sfn libsodium.so.23.3.0 "$ROOT/lib/x86_64-linux-gnu/libsodium.so.23" 2>/dev/null
# vim 二进制（vim.basic + alternatives 链）
[ -e /usr/bin/vim.basic ] && cp -a /usr/bin/vim.basic "$ROOT/usr/bin/" 2>/dev/null
mkdir -p "$ROOT/etc/alternatives"
[ -e /etc/alternatives/vim ] && cp -aP /etc/alternatives/vim "$ROOT/etc/alternatives/" 2>/dev/null
[ -e /etc/alternatives/vi ] && cp -aP /etc/alternatives/vi "$ROOT/etc/alternatives/" 2>/dev/null
[ -e /usr/bin/vim ] && cp -aP /usr/bin/vim "$ROOT/usr/bin/" 2>/dev/null
[ -e /usr/bin/vi ] && cp -aP /usr/bin/vi "$ROOT/usr/bin/" 2>/dev/null

# === 最终安全加固：清除任何 setuid/setgid 位（含上一步补入的二进制）===
find "$ROOT" -perm -4000 -type f -exec chmod -s {} + 2>/dev/null || true
find "$ROOT" -perm -2000 -type f -exec chmod -s {} + 2>/dev/null || true
echo ">>> rootfs 构建全部完成"
