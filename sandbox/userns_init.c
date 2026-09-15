//go:build ignore

/*
 * userns-init.c —— 单线程沙箱命名空间初始化器（sandbox 的 C 辅助）
 *
 * 背景：Go 程序多线程调用 unshare(CLONE_NEWPID) 后，fork 出的子进程才会成为
 * 新 PID namespace 的 init；而 Go 运行时是多线程的，无法安全地使用原生 fork()。
 * 因此把"unshare(NS/PID/NET) + make-rprivate + fork"放到这个极小的单线程 C
 * 程序里完成，完成后 exec 回 Go 的 sandbox-init --child 继续做 buildRootFS /
 * pivot_root / seccomp / 降权 / exec。
 *
 * 方案 A：不再创建 user namespace。本程序由 sandbox-init 经 sudo 以宿主 root
 * 身份启动，保持 root 身份完成 unshare 与 make-rprivate；随后 child 同样以 root
 * 完成 buildRootFS/pivot，最后在 Go childInit 内降权到目标真实用户并保留补充组
 * （setgroups + setgid + setuid），从而解决 userns 只映射主 uid/gid、导致 NFS
 * 因丢失补充组而 Permission denied 的问题。沙箱内进程最终是宿主目标用户
 * （uid/gid/补充组与宿主机一致），而非 root，也非 userns root。
 *
 * 调用方式（由 sandbox-init 父进程以宿主 root 身份调用）：
 *   userns-init \
 *     --uid <uid> --gid <gid> \
 *     --session <session> \
 *     --cgroup-root <path> \
 *     [--no-net] \
 *     -- <go-child-program> <go-child-args...>
 *
 * uid/gid/session 仅作参数校验保留（降权由 Go child 完成，本程序不再降权）。
 * 进入 cgroup 由 Go 父进程以宿主 root 完成，本程序（及其 fork 出的子进程）
 * 继承同一 cgroup。由于 unshare(CLONE_NEWNS) 后新 mount namespace 里
 * /sys/fs/cgroup 不可见，无法按路径删除 cgroup 目录，因此本程序在 unshare
 * 之前 fork 一个 cleaner 进程（保持宿主 mount ns），主流程 wait 回收子进程后
 * 通过管道通知 cleaner，由 cleaner 以宿主 root 删除该会话的 cgroup 目录
 * （<cgroup-root>/<session>），避免空目录无限累积。删除前需先把仍在该 cgroup
 * 内的会话进程移到根 cgroup（/sys/fs/cgroup），否则 rmdir 会因 EBUSY 失败。
 */
#define _GNU_SOURCE
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>
#include <errno.h>
#include <fcntl.h>
#include <sched.h>
#include <sys/mount.h>
#include <sys/types.h>
#include <sys/wait.h>

static void die(const char *m) {
    fprintf(stderr, "userns-init: %s: %s\n", m, strerror(errno));
    _exit(1);
}

/*
 * cleanup_cgroup —— best-effort 删除 <cgroup-root>/<session>。
 *
 * cgroup 目录删除失败（EBUSY）的根因是会话进程（含 C 程序主进程）仍在该
 * cgroup 内。实验结论：把 pid 写入直接父级 webshell_sandbox/cgroup.procs 会报
 * Device or resource busy，写入根 cgroup /sys/fs/cgroup/cgroup.procs 则成功；
 * 把目标 cgroup 内全部 pid 移到根后 rmdir 立即成功。
 *
 * 根 cgroup 路径由 cgroup_root 推导：cgroup_root 形如
 * /sys/fs/cgroup/webshell_sandbox，去掉最后一个路径分量即为其层级根
 * /sys/fs/cgroup（其根 cgroup.procs 即 /sys/fs/cgroup/cgroup.procs）。
 */
static void cleanup_cgroup(const char *cgroup_root, const char *session) {
    char path[PATH_MAX];
    int n = snprintf(path, sizeof(path), "%s/%s", cgroup_root, session);
    if (n < 0 || (size_t)n >= sizeof(path)) {
        fprintf(stderr, "userns-init: WARN: cgroup path too long, skip cleanup\n");
        return;
    }

    /* 推导根 cgroup 路径：去掉 cgroup_root 最后一个路径分量 */
    char root_path[PATH_MAX];
    int rn = snprintf(root_path, sizeof(root_path), "%s", cgroup_root);
    if (rn < 0 || (size_t)rn >= sizeof(root_path)) {
        fprintf(stderr, "userns-init: WARN: cgroup root path too long, skip cleanup\n");
        return;
    }
    size_t rlen = strlen(root_path);
    while (rlen > 1 && root_path[rlen - 1] == '/') root_path[--rlen] = '\0';
    char *slash = strrchr(root_path, '/');
    if (slash == root_path) slash[1] = '\0';  /* "/foo" -> "/" */
    else if (slash) *slash = '\0';            /* /sys/fs/cgroup/webshell_sandbox -> /sys/fs/cgroup */

    char target[PATH_MAX];
    int tn = snprintf(target, sizeof(target), "%s/cgroup.procs", root_path);
    if (tn < 0 || (size_t)tn >= sizeof(target)) {
        fprintf(stderr, "userns-init: WARN: root cgroup path too long, skip cleanup\n");
        return;
    }

    /* 循环重试：把该 cgroup 内全部 pid 移到根 cgroup 后 rmdir，最多约 10 次 */
    int rc = -1;
    int saved_errno = 0;
    for (int attempt = 0; attempt < 10; attempt++) {
        /* 1) 读取目标 cgroup 的全部 pid */
        char procs[PATH_MAX];
        int pn = snprintf(procs, sizeof(procs), "%s/cgroup.procs", path);
        if (pn >= 0 && (size_t)pn < sizeof(procs)) {
            FILE *fp = fopen(procs, "r");
            if (fp) {
                char line[64];
                while (fgets(line, sizeof(line), fp)) {
                    long pid = atol(line);
                    if (pid <= 0) continue;
                    /* 2) 逐个写入根 cgroup 的 cgroup.procs 移出 */
                    int fd = open(target, O_WRONLY);
                    if (fd < 0) continue;
                    char buf[32];
                    int len = snprintf(buf, sizeof(buf), "%ld\n", pid);
                    if (len > 0) (void)!write(fd, buf, (size_t)len);
                    close(fd);
                }
                fclose(fp);
            }
        }
        /* 3) rmdir，成功后结束重试 */
        if ((rc = rmdir(path)) == 0) break;
        saved_errno = errno;
        if (attempt < 9) usleep(200 * 1000);
    }
    if (rc < 0) {
        fprintf(stderr, "userns-init: WARN: rmdir %s: %s\n", path, strerror(saved_errno));
    }
}

int main(int argc, char **argv) {
    long uid = -1, gid = -1;
    const char *session = NULL;
    const char *cgroup_root = NULL;
    int no_net = 0;
    int i = 1;

    /* 解析参数直到 -- */
    for (; i < argc; i++) {
        if (strcmp(argv[i], "--") == 0) { i++; break; }
        else if (strcmp(argv[i], "--uid") == 0 && i + 1 < argc) uid = atol(argv[++i]);
        else if (strcmp(argv[i], "--gid") == 0 && i + 1 < argc) gid = atol(argv[++i]);
        else if (strcmp(argv[i], "--session") == 0 && i + 1 < argc) session = argv[++i];
        else if (strcmp(argv[i], "--cgroup-root") == 0 && i + 1 < argc) cgroup_root = argv[++i];
        else if (strcmp(argv[i], "--no-net") == 0) no_net = 1;
        else { fprintf(stderr, "userns-init: unknown arg: %s\n", argv[i]); return 1; }
    }

    if (uid <= 0 || gid <= 0 || !session || i >= argc) {
        fprintf(stderr, "userns-init: missing required args\n");
        return 1;
    }

    /*
     * ---------- 0. fork cleaner：必须在 unshare 之前 ----------
     * unshare(CLONE_NEWNS) 后新 mount namespace 里 /sys/fs/cgroup 不可见，
     * 无法按路径（或用 fd）删除 cgroup 目录。因此先 fork 一个 cleaner 进程，
     * 它保持宿主 mount ns，阻塞等待主流程结束信号，收到后以宿主 root 按路径
     * rmdir 该会话的 cgroup 目录。
     */
    int pfd[2];
    if (pipe(pfd) < 0) die("pipe");
    pid_t cleaner_pid = fork();
    if (cleaner_pid < 0) die("fork(cleaner)");
    if (cleaner_pid == 0) {
        /* cleaner 子进程：保持宿主 mount ns，不进入新 ns */
        close(pfd[1]);
        char c;
        while (read(pfd[0], &c, 1) < 0 && errno == EINTR) {}
        close(pfd[0]);
        if (cgroup_root && session) cleanup_cgroup(cgroup_root, session);
        _exit(0);
    }
    /* 主进程关闭读端，保留写端用于通知 cleaner */
    close(pfd[0]);

    /* ---------- 1. 创建 mount/pid/net namespace（本进程保持宿主 root） ---------- */
    int flags = CLONE_NEWNS | CLONE_NEWPID;
    if (no_net) flags |= CLONE_NEWNET;
    if (unshare(flags) < 0) die("unshare(NEWNS|NEWPID|NEWNET)");

    /* ---------- 2. mount namespace 内所有挂载改为私有，避免影响宿主 ---------- */
    if (mount(NULL, "/", NULL, MS_REC | MS_PRIVATE, NULL) < 0)
        die("mount --make-rprivate /");

    /*
     * ---------- 3. fork 后 exec Go 子程序（sandbox-init --child）继续 ----------
     * CLONE_NEWPID 只对之后 fork 的子进程生效：fork 出的子进程成为新 PID
     * namespace 的 PID 1（init），并继承父进程已建立的 mount/net ns。
     * 父进程 wait 回收并传递退出码。
     */
    pid_t child_pid = fork();
    if (child_pid < 0) die("fork");
    if (child_pid > 0) {
        int st = 0;
        while (waitpid(child_pid, &st, 0) < 0 && errno == EINTR) {}
        int rc;
        if (WIFEXITED(st)) rc = WEXITSTATUS(st);
        else if (WIFSIGNALED(st)) rc = 128 + WTERMSIG(st);
        else rc = 1;

        /* ---------- 4. 通知 cleaner 删除 cgroup 目录并回收 ----------
         * cgroup 目录（<cgroup-root>/<session>）由 Go 父进程以宿主 root 创建，
         * 属主是 root，服务进程（ease，已降权）无权删除，导致空目录无限累积。
         * cleaner 在 unshare 之前 fork，保持宿主 mount ns，收到信号后以宿主
         * root rmdir。best-effort：目录非空 / 权限问题等失败仅打印 WARN 不致命。
         */
        char sig = 'x';
        if (write(pfd[1], &sig, 1) < 0) {
            fprintf(stderr, "userns-init: WARN: notify cleaner: %s\n", strerror(errno));
        }
        close(pfd[1]);
        while (waitpid(cleaner_pid, NULL, 0) < 0 && errno == EINTR) {}
        return rc;
    }
    /* 子进程：成为新 PID ns 的 PID 1 */
    execv(argv[i], &argv[i]);
    die("exec go-child");
    return 1;
}
