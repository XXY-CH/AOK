#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <string.h>
#include <linux/capability.h>
#include <unistd.h>

static void check(int yes, const char *label)
{
	if (!yes) { fprintf(stderr, "FAIL %s errno=%d\n", label, errno); exit(1); }
	printf("PASS %s\n", label);
}

int main(int argc, char **argv)
{
	if (argc != 5 && argc != 6) return 2;
	int network = argc == 6 && !strcmp(argv[5], "network");
	struct __user_cap_header_struct header = {_LINUX_CAPABILITY_VERSION_3, 0};
	struct __user_cap_data_struct caps[2];
	check(syscall(SYS_capget, &header, caps) == 0 && !caps[0].effective && !caps[1].effective && !caps[0].permitted && !caps[1].permitted && !caps[0].inheritable && !caps[1].inheritable, "Linux capabilities dropped");
	int fd = open(argv[1], O_RDONLY);
	check(fd >= 0, "authorized read"); close(fd);
	fd = open(argv[2], O_CREAT | O_WRONLY | O_TRUNC, 0600);
	check(fd >= 0, "authorized write"); close(fd);
	fd = open(argv[1], O_WRONLY | O_TRUNC);
	check(fd == -1 && errno == EACCES, "read grant cannot write");
	fd = open(argv[2], O_RDONLY);
	check(fd == -1 && errno == EACCES, "write grant cannot read");
	fd = open(argv[3], O_RDONLY);
	check(fd == -1 && errno == EACCES, "unauthorized read denied");
	fd = open(argv[3], O_WRONLY | O_TRUNC);
	check(fd == -1 && errno == EACCES, "unauthorized write denied");
	fd = open(argv[4], O_RDONLY);
	check(fd == -1 && errno == EACCES, "symlink escape denied");
	fd = socket(AF_INET, SOCK_STREAM, 0);
	if (network) { check(fd >= 0, "authorized IPv4 socket"); close(fd); }
	else check(fd == -1 && errno == EPERM, "network denied");
	fd = socket(AF_INET6, SOCK_DGRAM, 0);
	if (network) { check(fd >= 0, "authorized IPv6 socket"); close(fd); }
	else check(fd == -1 && errno == EPERM, "IPv6 denied");
	fd = socket(AF_UNIX, SOCK_STREAM, 0);
	check(fd == -1 && errno == EPERM, "Unix socket denied");
	int sv[2];
	check(socketpair(AF_UNIX, SOCK_STREAM, 0, sv) == -1 && errno == EPERM, "socketpair denied");
	check(syscall(SYS_unshare, 0) == -1 && errno == EPERM, "namespace denied");
	check(chmod(argv[3], 0777) == -1 && errno == EPERM, "metadata mutation denied");
	if (network) check(fcntl(9, F_GETFD) >= 0, "explicitly delegated fd retained");
	else check(fcntl(9, F_GETFD) == -1 && errno == EBADF, "inherited fd closed");
	pid_t pid = fork();
	if (!pid) { fd = open(argv[3], O_RDONLY); _exit(fd == -1 && errno == EACCES ? 0 : 1); }
	int status;
	check(pid > 0 && waitpid(pid, &status, 0) == pid && WIFEXITED(status) && !WEXITSTATUS(status), "child inherits restriction");
	puts("AOK_SANDBOX_OS_ENFORCEMENT_OK");
	return 0;
}
