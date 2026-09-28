#!/bin/bash
# 构建最小可运行 rootfs（RHEL 9.x / Rocky / AlmaLinux / CentOS Stream 9）
# 目标：/opt/runner/rootfs/base
#
# 特点（相对 Ubuntu 版）：
#   - 库目录为 /usr/lib64、/lib64（无 multiarch）
#   - Python 3.9（libpython3.9.so.1.0，位于 /usr/lib64）
#   - gcc 无 -11 后缀；vim / vi 为真实二进制（无 alternatives / vim.basic）
#   - locale 为目录式（/usr/lib/locale/<lang>），通常无 locale-archive
#   - NSS(sssd) 模块位于 /usr/lib64
#   - vim 依赖 libselinux/libacl/libattr/libgpm/libpcre2
#   - SELinux：本脚本不负责 relabel；确认 SELinux 为 Permissive/Disabled，
#     或对 rootfs 执行 restorecon（见 DEPLOY.md「RHEL 差异」）
#
# 从宿主拷贝二进制 + ldd 递归依赖；并补入 NSS(sssd)、locale、用户/组、去 setuid。
# 必须在目标服务器（RHEL9）上以 root 运行。
# 目标目录可用环境变量覆盖：ROOT=/tmp/xx bash build_rootfs_rhel.sh
set -euo pipefail

ROOT="${ROOT:-/opt/runner/rootfs/base}"

echo ">>> 清空并重建目录结构"
rm -rf "$ROOT"
mkdir -p "$ROOT"/{bin,sbin,usr/bin,usr/sbin,usr/lib,usr/lib64,lib,lib64,etc,home,tmp,proc,dev,dev/pts,dev/shm,root,var/tmp,opt,usr/lib/locale}
chmod 1777 "$ROOT"/tmp "$ROOT"/var/tmp
chmod 1777 "$ROOT"/dev/shm

# 需拷贝的二进制（核心工具 + 编译运行工具；RHEL 路径）
BINS=(
  /bin/bash /bin/sh /usr/bin/bash
  /usr/bin/ls /usr/bin/cat /usr/bin/grep /usr/bin/awk /usr/bin/sed
  /usr/bin/mkdir /usr/bin/cp /usr/bin/rm /usr/bin/mv /usr/bin/ln
  /usr/bin/touch /usr/bin/pwd /usr/bin/whoami /usr/bin/id /usr/bin/uname
  /usr/bin/ps /usr/bin/top /usr/bin/env /usr/bin/find /usr/bin/xargs
  /usr/bin/head /usr/bin/tail /usr/bin/wc /usr/bin/sort /usr/bin/uniq
  /usr/bin/cut /usr/bin/tr /usr/bin/tee /usr/bin/diff /usr/bin/patch
  /usr/bin/make /usr/bin/gcc /usr/bin/cc /usr/bin/as /usr/bin/ld
  /usr/bin/python3 /usr/bin/python3.9 /usr/bin/perl
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
    case "$src" in /bin/*|/sbin/*) dest="/bin/$name";; esac
    ln -sfn "$target" "$ROOT$dest" 2>/dev/null || true
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
      *'/lib/'*|*'/lib64/'*|*'/usr/lib/'*|*'/usr/lib64/'*)
        lib="$(echo "$line" | grep -oE '/[^ ]+\.so[^ ]*' | head -1 || true)"
        ;;
      *) continue;;
    esac
    [ -z "$lib" ] && continue
    [ -e "$lib" ] || continue
    libname="$(basename "$lib")"
    if [ -n "${SEEN[$lib]:-}" ]; then continue; fi
    SEEN["$lib"]=1
    local dest="${lib#/}"
    local destfull="$ROOT/$dest"
    mkdir -p "$(dirname "$destfull")"
    if [ -L "$lib" ]; then
      [ -e "$destfull" ] || cp -aP "$lib" "$destfull" 2>/dev/null || true
      local real
      real="$(readlink -f "$lib")"
      [ -n "$real" ] || return 0
      local linkdir
      linkdir="$(dirname "$lib")"
      mkdir -p "$ROOT/${linkdir#/}"
      [ -e "$ROOT/${linkdir#/}/$(basename "$real")" ] || cp -a "$real" "$ROOT/${linkdir#/}/$(basename "$real")" 2>/dev/null || true
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

echo ">>> 拷贝常用动态库（RHEL：/usr/lib64）"
LIB_DIRS=(/usr/lib64 /lib64)
COMMON_LIBS=(
  libgcc_s.so.1 libstdc++.so.6
  libpython3.9.so.1.0 libpython3.so
  libcrypto.so.3 libssl.so.3
  libz.so.1 libffi.so.8 libexpat.so.1
  libmpc.so.3 libmpfr.so.6 libgmp.so.10 libisl.so.23
  libzstd.so.1 libbz2.so.1 libbz2.so.1.0.8 liblzma.so.5
  libreadline.so.8 libtinfo.so.6 libsqlite3.so.0 libuuid.so.1
  libpcre2-8.so.0 libpthread.so.0 libdl.so.2 librt.so.1
  libselinux.so.1 libacl.so.1 libattr.so.1 libgpm.so.2
)
for name in "${COMMON_LIBS[@]}"; do
  for d in "${LIB_DIRS[@]}"; do
    [ -e "$d/$name" ] || continue
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
# AD/LDAP 用户经 sssd（nsswitch: passwd: sss files systemd）解析，rootfs 需含
# libnss_sss.so.2（及 libnss_systemd.so.2）才能按名解析 AD 用户/组；配合
# sandbox.bind_mounts 把 /var/lib/sss/pipes 与 /var/lib/sss/mc 映射进沙箱。
# 注意：宿主 sssd 服务须处于 active；SELinux 须 Permissive/Disabled。
for mod in libnss_sss.so.2 libnss_systemd.so.2; do
  for d in /usr/lib64 /lib64; do
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
# 动态链接器 /lib64/ld-linux-x86-64.so.2（RHEL 为真实文件）
if [ ! -e "$ROOT/lib64/ld-linux-x86-64.so.2" ] && [ -e /lib64/ld-linux-x86-64.so.2 ]; then
  cp -aP /lib64/ld-linux-x86-64.so.2 "$ROOT/lib64/" 2>/dev/null || true
fi
mkdir -p "$ROOT/lib64" "$ROOT/usr/lib64"

echo ">>> 拷贝 vim / vi（RHEL 为真实二进制）"
for b in /usr/bin/vim /usr/bin/vi; do
  [ -e "$b" ] && cp -a "$b" "$ROOT/usr/bin/" 2>/dev/null || true
done
# vim 依赖（libselinux/libacl/libattr/libgpm/libpcre2 等）已由 COMMON_LIBS 覆盖；
# 另对 vim 直接做一次 ldd 依赖递归，确保不遗漏。
if [ -e /usr/bin/vim ]; then copy_dep /usr/bin/vim; fi

echo ">>> 写入/拷贝 /etc 文件"
# 用户/组：优先拷贝宿主 /etc/passwd、/etc/group，使沙箱内 id/prompt 能解析真实用户名
# （AD 用户经 sssd 动态解析，需配合 libnss_sss + /var/lib/sss 绑定）。宿主缺失时回退最小集合。
if [ -e /etc/passwd ]; then
  cp -a /etc/passwd "$ROOT/etc/passwd"
else
  cat > "$ROOT/etc/passwd" <<'EOF'
root:x:0:0:root:/root:/bin/bash
nobody:x:65534:65534:nobody:/nonexistent:/sbin/nologin
EOF
fi
if [ -e /etc/group ]; then
  cp -a /etc/group "$ROOT/etc/group"
else
  cat > "$ROOT/etc/group" <<'EOF'
root:x:0:
nobody:x:65534:
EOF
fi
[ -e /etc/nsswitch.conf ] && cp -a /etc/nsswitch.conf "$ROOT/etc/nsswitch.conf" 2>/dev/null || true
: > "$ROOT/etc/mtab"
: > "$ROOT/etc/fstab"
echo "127.0.0.1 localhost" > "$ROOT/etc/hosts"
echo "nameserver 8.8.8.8" > "$ROOT/etc/resolv.conf" 2>/dev/null || true
printf 'root:x:0:0:root:/root:/bin/bash\n' > "$ROOT/etc/shadow" 2>/dev/null || true

echo ">>> 拷贝 locale（目录式；RHEL 通常无 locale-archive）"
mkdir -p "$ROOT/usr/lib/locale"
[ -e /usr/lib/locale/locale-archive ] && cp -a /usr/lib/locale/locale-archive "$ROOT/usr/lib/locale/" 2>/dev/null || true
for d in C.utf8 en_US.utf8 en_US; do
  [ -e "/usr/lib/locale/$d" ] && cp -a "/usr/lib/locale/$d" "$ROOT/usr/lib/locale/" 2>/dev/null || true
done
[ -e /usr/lib/locale/locale.alias ] && cp -a /usr/lib/locale/locale.alias "$ROOT/usr/lib/locale/" 2>/dev/null || true

echo ">>> 去除 setuid/setgid 位（沙箱内不允许提权）"
find "$ROOT" -perm -4000 -type f -exec chmod -s {} + 2>/dev/null || true
find "$ROOT" -perm -2000 -type f -exec chmod -s {} + 2>/dev/null || true

echo ">>> 完成"
du -sh "$ROOT"
echo ">>> bash 是否可运行（chroot 测试，需 root）"
chroot "$ROOT" /bin/bash -c 'echo OK bash works; /usr/bin/id; /usr/bin/ls /' 2>&1 | head -20
