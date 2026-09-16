/* SPDX-License-Identifier: Apache-2.0 */
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/audit.h>
#include <linux/capability.h>
#include <linux/filter.h>
#include <linux/landlock.h>
#include <linux/sched.h>
#include <linux/seccomp.h>
#include <stddef.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

#if defined(__aarch64__)
#define NATIVE_ARCH AUDIT_ARCH_AARCH64
#elif defined(__x86_64__)
#define NATIVE_ARCH AUDIT_ARCH_X86_64
#else
#error Unsupported sandbox architecture
#endif

#define FS_EXEC (1ULL << 0)
#define FS_WRITE (1ULL << 1)
#define FS_READ (1ULL << 2)
#define FS_DIR (1ULL << 3)
#define FS_REFER (1ULL << 13)
#define FS_TRUNCATE (1ULL << 14)
#define FS_ALL ((1ULL << 16) - 1)
#define FS_MUTATE (FS_WRITE | FS_TRUNCATE | FS_REFER | (0x1ffULL << 4))
#define DENY(nr) BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K, nr, 0, 1), BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_ERRNO|EPERM)

static void die(const char *what) { perror(what); exit(125); }

static void rule(int ruleset, const char *path, uint64_t rights)
{
	if (path[0] != '/') { errno = EINVAL; die("absolute rule path required"); }
	int fd = open(path, O_PATH | O_CLOEXEC);
	if (fd < 0) die("open sandbox rule");
	struct stat st;
	if (fstat(fd, &st)) die("stat sandbox rule");
	if (!S_ISDIR(st.st_mode)) rights &= FS_EXEC | FS_READ | FS_WRITE | FS_TRUNCATE;
	struct landlock_path_beneath_attr attr = {.allowed_access = rights, .parent_fd = fd};
	if (syscall(SYS_landlock_add_rule, ruleset, LANDLOCK_RULE_PATH_BENEATH, &attr, 0)) die("landlock_add_rule");
	close(fd);
}

static void filter(int network)
{
	struct sock_filter code[] = {
		BPF_STMT(BPF_LD|BPF_W|BPF_ABS, offsetof(struct seccomp_data, arch)),
		BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K, NATIVE_ARCH, 1, 0),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_KILL_PROCESS),
		BPF_STMT(BPF_LD|BPF_W|BPF_ABS, offsetof(struct seccomp_data, nr)),
#if defined(__x86_64__)
		BPF_JUMP(BPF_JMP|BPF_JGE|BPF_K, 0x40000000, 0, 1),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_KILL_PROCESS),
#endif
		DENY(SYS_ptrace), DENY(SYS_process_vm_readv), DENY(SYS_process_vm_writev),
		DENY(SYS_pidfd_getfd), DENY(SYS_mount), DENY(SYS_umount2),
		DENY(SYS_pivot_root), DENY(SYS_chroot), DENY(SYS_unshare), DENY(SYS_setns),
		DENY(SYS_open_by_handle_at), DENY(SYS_name_to_handle_at),
		DENY(SYS_bpf), DENY(SYS_perf_event_open), DENY(SYS_userfaultfd),
		DENY(SYS_io_uring_setup), DENY(SYS_io_uring_enter), DENY(SYS_io_uring_register),
		DENY(SYS_keyctl), DENY(SYS_add_key), DENY(SYS_request_key),
		DENY(SYS_init_module), DENY(SYS_finit_module), DENY(SYS_delete_module),
		DENY(SYS_reboot), DENY(SYS_kexec_load), DENY(SYS_swapon), DENY(SYS_swapoff),
		DENY(SYS_fsopen), DENY(SYS_fsconfig), DENY(SYS_fsmount),
		DENY(SYS_move_mount), DENY(SYS_open_tree), DENY(SYS_mount_setattr),
		DENY(SYS_mknodat), DENY(SYS_fchmod), DENY(SYS_fchmodat),
		DENY(SYS_fchown), DENY(SYS_fchownat),
		DENY(SYS_setxattr), DENY(SYS_lsetxattr), DENY(SYS_fsetxattr),
		DENY(SYS_removexattr), DENY(SYS_lremovexattr), DENY(SYS_fremovexattr),
#ifdef SYS_chmod
		DENY(SYS_chmod), DENY(SYS_chown), DENY(SYS_lchown), DENY(SYS_mknod),
#endif
		/* fchmodat2 was added after Debian bookworm's userspace headers. */
		DENY(452),
		DENY(SYS_socketpair),
		/* clone3 has an indirect flags pointer: force libc's clone fallback. */
		BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K, SYS_clone3, 0, 1),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_ERRNO|ENOSYS),
		BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K, SYS_clone, 0, 4),
		BPF_STMT(BPF_LD|BPF_W|BPF_ABS, offsetof(struct seccomp_data,args[0])),
		BPF_JUMP(BPF_JMP|BPF_JSET|BPF_K, CLONE_NEWNS|CLONE_NEWCGROUP|CLONE_NEWUTS|CLONE_NEWIPC|CLONE_NEWUSER|CLONE_NEWPID|CLONE_NEWNET, 0, 1),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_ERRNO|EPERM),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_ALLOW),
		BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K, SYS_socket, 0, 6),
		BPF_STMT(BPF_LD|BPF_W|BPF_ABS, offsetof(struct seccomp_data,args[0])),
		BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K, network ? AF_INET : UINT32_MAX, 2, 0),
		BPF_JUMP(BPF_JMP|BPF_JEQ|BPF_K, network ? AF_INET6 : UINT32_MAX, 1, 0),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_ERRNO|EPERM),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_ALLOW),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_ERRNO|EPERM),
		BPF_STMT(BPF_RET|BPF_K, SECCOMP_RET_ALLOW),
	};
	struct sock_fprog prog = {.len = sizeof(code)/sizeof(code[0]), .filter = code};
	if (prctl(PR_SET_SECCOMP, SECCOMP_MODE_FILTER, &prog)) die("seccomp");
}

int main(int argc, char **argv)
{
	int abi = syscall(SYS_landlock_create_ruleset, NULL, 0, LANDLOCK_CREATE_RULESET_VERSION);
	if (abi < 6) { errno = ENOTSUP; die("Landlock ABI 6 required"); }
	/* ABI 6 also scopes signals and abstract Unix sockets to this domain. */
	struct { uint64_t fs, net, scoped; } attr = {FS_ALL, 0, 3};
	int ruleset = syscall(SYS_landlock_create_ruleset, &attr, sizeof(attr), 0);
	if (ruleset < 0) die("landlock_create_ruleset");
	int network = 0, cmd = 0;
	uint64_t keep = 0;
	for (int i = 1; i < argc; i++) {
		if (!strcmp(argv[i], "--")) { cmd = i + 1; break; }
		if (!strcmp(argv[i], "--net")) { network = 1; continue; }
		if (!strcmp(argv[i], "--keep-fd")) {
			if (++i >= argc) { errno = EINVAL; die("missing delegated fd"); }
			char *end;
			long fd = strtol(argv[i], &end, 10);
			if (*end || fd < 3 || fd > 63 || fcntl(fd, F_GETFD) < 0) { errno = EINVAL; die("invalid delegated fd"); }
			keep |= 1ULL << fd;
			continue;
		}
		uint64_t rights;
		if (!strcmp(argv[i], "--read")) rights = FS_READ | FS_DIR;
		else if (!strcmp(argv[i], "--write")) rights = FS_MUTATE;
		else if (!strcmp(argv[i], "--execute")) rights = FS_READ | FS_EXEC;
		else { errno = EINVAL; die("unknown sandbox option"); }
		if (++i >= argc) { errno = EINVAL; die("missing rule path"); }
		rule(ruleset, argv[i], rights);
	}
	if (!cmd || cmd >= argc || argv[cmd][0] != '/') { errno = EINVAL; die("missing absolute sandbox command"); }
	rule(ruleset, argv[cmd], FS_READ | FS_EXEC);
	if (prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0)) die("no_new_privs");
	struct __user_cap_header_struct cap_header = {_LINUX_CAPABILITY_VERSION_3, 0};
	struct __user_cap_data_struct cap_data[2] = {{0}};
	if (syscall(SYS_capset, &cap_header, cap_data)) die("drop capabilities");
	if (syscall(SYS_landlock_restrict_self, ruleset, 0)) die("landlock_restrict_self");
	close(ruleset);
	/* Explicitly delegated descriptors are capabilities outside path rules. */
	for (unsigned int fd = 3; fd < 64; fd++) if (!(keep & (1ULL << fd))) close(fd);
	if (syscall(SYS_close_range, 64, ~0U, 0)) die("close_range");
	if (chdir("/")) die("chdir");
	filter(network);
	execv(argv[cmd], argv + cmd);
	die("exec sandbox command");
}
