// SPDX-License-Identifier: GPL-2.0-only
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/aok.h>
#include <pthread.h>
#include <sched.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#include "kselftest.h"

#ifndef __aarch64__
#error "This experimental syscall allocation is tested on arm64 only"
#endif

#define NR_AOK_CREATE __NR_aok_aproc_create
#define NR_AOK_INSPECT __NR_aok_object_inspect
#define NR_AOK_DUPLICATE __NR_aok_handle_duplicate
#define NR_AOK_ROOT_CLAIM __NR_aok_root_cap_claim

_Static_assert(sizeof(struct aok_aproc_create_args) == 40, "create layout");
_Static_assert(sizeof(struct aok_object_info) == 56, "inspect layout");

static struct aok_aproc_create_args args = {
	.size = sizeof(args), .parent_job_fd = -1,
};
static int root_fd = -1;

static struct aok_aproc_create_result create_result;

static void fork_child_brief(void)
{
	/* First tasks of this slice are forks of the caller; stay alive
	 * briefly so inspect results stay deterministic, then exit. */
	usleep(500000);
	_exit(0);
}

static int create(struct aok_object_info *info)
{
	long r;

	args.parent_job_fd = root_fd;
	info->size = sizeof(*info);
	create_result.size = sizeof(create_result);
	r = syscall(NR_AOK_CREATE, &args, info, &create_result);
	if (r == 0)
		fork_child_brief();
	return r;
}

static int inspect(int fd, struct aok_object_info *info)
{
	info->size = sizeof(*info);
	return syscall(NR_AOK_INSPECT, fd, info);
}

static int narrow(int fd, uint64_t rights)
{
	return syscall(NR_AOK_DUPLICATE, fd, rights);
}

#define EXPECT_ERR(expr, error, name) do { \
	errno = 0; \
	long result = (expr); \
	int saved_errno = errno; \
	ksft_test_result(result == -1 && saved_errno == (error), \
			 "%s (ret=%ld errno=%d)\n", name, result, saved_errno); \
	if (result >= 0 && result != 0) close(result); \
} while (0)

static void *duplicate_worker(void *arg)
{
	int original = *(int *)arg;
	struct aok_object_info info;
	int i;

	for (i = 0; i < 1000; i++) {
		int fd = narrow(original, AOK_RIGHT_INSPECT);

		if (fd < 0)
			return (void *)1;
		if (inspect(fd, &info) || info.rights != AOK_RIGHT_INSPECT) {
			close(fd);
			return (void *)1;
		}
		close(fd);
	}
	return NULL;
}

static void *close_worker(void *arg)
{
	int fd = *(int *)arg;
	struct aok_object_info info;
	bool ok = !inspect(fd, &info);

	return close(fd) || !ok ? (void *)1 : NULL;
}

static void finish(int status)
{
	printf("AOK_OBJECT_TEST=%s\n", status ? "fail" : "pass");
	fflush(stdout);
	if (getpid() == 1) {
		reboot(RB_POWER_OFF);
		for (;;)
			pause();
	}
	exit(status);
}

int main(int argc, char **argv)
{
	struct aok_object_info first = {}, info = {};
	int fd, child_fd, empty_fd, raw_fd, status, saved;
	uint64_t previous;
	pid_t pid;
	long page = sysconf(_SC_PAGESIZE);
	char *guard;
	bool ok;
	pthread_t threads[4];
	unsigned int started = 0, i;
	int closing[4];
	struct rlimit original_limit, small_limit;
	struct {
		struct aok_aproc_create_args args;
		uint64_t tail;
	} extended = { .args = args };
	struct {
		struct aok_object_info info;
		uint64_t tail;
	} extended_info;

	if (getpid() == 1) {
		mkdir("/proc", 0755);
		mkdir("/dev", 0755);
		mount("proc", "/proc", "proc", 0, NULL);
		mount("devtmpfs", "/dev", "devtmpfs", 0, NULL);
	}
	setvbuf(stdout, NULL, _IONBF, 0);
	if (argc == 3 && !strcmp(argv[1], "--check-closed")) {
		int inherited = atoi(argv[2]);

		errno = 0;
		return fcntl(inherited, F_GETFD) == -1 && errno == EBADF ? 0 : 1;
	}
	ksft_print_header();
	if (argc == 2 && !strcmp(argv[1], "--expect-disabled")) {
		ksft_set_plan(4);
		EXPECT_ERR(syscall(NR_AOK_ROOT_CLAIM), ENOSYS, "disabled root claim");
		EXPECT_ERR(create(&info), ENOSYS, "disabled create");
		EXPECT_ERR(inspect(-1, &info), ENOSYS, "disabled inspect");
		EXPECT_ERR(narrow(-1, 0), ENOSYS, "disabled duplicate");
		ksft_print_cnts();
		finish(ksft_get_fail_cnt() != 0);
	}
	if (getpid() != 1)
		ksft_exit_skip("run as initial PID 1 in the QEMU test initramfs\n");
	ksft_set_plan(44);
	root_fd = syscall(NR_AOK_ROOT_CLAIM);
	ksft_test_result(root_fd >= 0, "PID1 claims root capability once\n");
	ksft_test_result(syscall(NR_AOK_ROOT_CLAIM) == -1 && errno == EALREADY,
			 "root capability cannot be claimed twice\n");
	fd = create(&first);
	ksft_test_result(fd >= 0, "create anon-inode object\n");
	if (fd < 0)
		finish(1);
	ksft_test_result(first.aid && first.type == AOK_OBJECT_APROC &&
			 first.state == AOK_APROC_STATE_RUNNING &&
			 first.rights == AOK_RIGHTS_APROC_INITIAL,
			 "created object has identity and initial rights\n");
	ksft_test_result(fcntl(fd, F_GETFD) == FD_CLOEXEC, "create CLOEXEC\n");
	memset(&info, 0xff, sizeof(info));
	ksft_test_result(!inspect(fd, &info) && !memcmp(&info, &first, sizeof(info)),
			 "inspect consistent and reserved bytes zero\n");
	EXPECT_ERR(inspect(-1, &info), EBADF, "invalid fd");
	raw_fd = open("/dev/null", O_RDWR);
	EXPECT_ERR(inspect(raw_fd, &info), EINVAL, "non-AOK fd");
	EXPECT_ERR(narrow(raw_fd, 0), EINVAL, "duplicate non-AOK fd");
	close(raw_fd);
	EXPECT_ERR(syscall(NR_AOK_CREATE, NULL, &info, &create_result), EFAULT, "null create args");
	EXPECT_ERR(syscall(NR_AOK_CREATE, &args, NULL, &create_result), EFAULT, "null create result");
	EXPECT_ERR(syscall(NR_AOK_CREATE, &args, &info, NULL), EFAULT, "null create task result");
	EXPECT_ERR(syscall(NR_AOK_INSPECT, fd, NULL), EFAULT, "null inspect result");
	args.size--;
	EXPECT_ERR(create(&info), EINVAL, "short create args");
	args.size = page + 1;
	EXPECT_ERR(create(&info), EINVAL, "oversized create args");
	args.size = sizeof(args);
	args.flags = 1;
	EXPECT_ERR(create(&info), EINVAL, "unknown flags");
	args.flags = 0;
	args.reserved[2] = 1;
	EXPECT_ERR(create(&info), EINVAL, "reserved field");
	args.reserved[2] = 0;
	args.parent_job_fd = -1;
	EXPECT_ERR(syscall(NR_AOK_CREATE, &args, &info, &create_result), EBADF,
		   "invalid parent capability");
	args.parent_job_fd = root_fd;
	info.size = sizeof(info) - 1;
	EXPECT_ERR(syscall(NR_AOK_INSPECT, fd, &info), EINVAL, "short inspect buffer");
	extended.args.size = sizeof(extended);
	extended.args.parent_job_fd = root_fd;
	info.size = sizeof(info);
	child_fd = syscall(NR_AOK_CREATE, &extended, &info, &create_result);
	if (child_fd == 0)
		fork_child_brief();
	ksft_test_result(child_fd >= 0, "zero extended args accepted\n");
	if (child_fd >= 0)
		close(child_fd);
	extended.tail = 1;
	EXPECT_ERR(syscall(NR_AOK_CREATE, &extended, &info, &create_result), E2BIG,
		   "unknown nonzero tail rejected");
	memset(&extended_info, 0xff, sizeof(extended_info));
	extended_info.info.size = sizeof(extended_info);
	ksft_test_result(!syscall(NR_AOK_INSPECT, fd, &extended_info) &&
			 extended_info.tail == 0, "extended output zero filled\n");
	child_fd = narrow(fd, AOK_RIGHT_INSPECT);
	ksft_test_result(child_fd >= 0 && !inspect(child_fd, &info) &&
			 info.aid == first.aid && info.rights == AOK_RIGHT_INSPECT,
			 "duplicate narrows rights and keeps identity\n");
	ksft_test_result(child_fd >= 0 && fcntl(child_fd, F_GETFD) == FD_CLOEXEC,
			 "duplicate CLOEXEC\n");
	EXPECT_ERR(narrow(child_fd, AOK_RIGHT_INSPECT), EACCES, "duplicate right required");
	EXPECT_ERR(narrow(fd, 1ULL << 63), EACCES, "cannot add rights");
	empty_fd = narrow(fd, AOK_RIGHT_DUPLICATE);
	EXPECT_ERR(inspect(empty_fd, &info), EACCES, "inspect right required");
	EXPECT_ERR(narrow(empty_fd, AOK_RIGHTS_INITIAL), EACCES, "cannot regain inspect");
	raw_fd = dup(child_fd);
	ksft_test_result(raw_fd >= 0 && !inspect(raw_fd, &info) &&
			 info.rights == AOK_RIGHT_INSPECT, "Linux dup preserves grant\n");
	close(raw_fd);
	close(empty_fd);
	pid = fork();
	if (!pid) {
		int inherited_ok = !inspect(child_fd, &info) && info.aid == first.aid;
		int ret = create(&info);

		_exit(inherited_ok && ret >= 0 ? 0 : 1);
	}
	ksft_test_result(pid > 0 && waitpid(pid, &status, 0) == pid &&
			 WIFEXITED(status) && WEXITSTATUS(status) == 0,
			 "child inherits root capability and can bootstrap\n");
	pid = fork();
	if (!pid) {
		char number[32];

		snprintf(number, sizeof(number), "%d", child_fd);
		execl("/init", "/init", "--check-closed", number, NULL);
		_exit(1);
	}
	ksft_test_result(pid > 0 && waitpid(pid, &status, 0) == pid &&
			 WIFEXITED(status) && WEXITSTATUS(status) == 0,
			 "exec closes capability fd\n");
	pid = fork();
	if (!pid) {
		pid_t nested;

		if (unshare(CLONE_NEWPID))
			_exit(1);
		nested = fork();
		if (!nested) {
			int ret = create(&info);

			_exit(getpid() == 1 && ret >= 0 ? 0 : 1);
		}
		if (nested < 0 || waitpid(nested, &status, 0) != nested)
			_exit(1);
		_exit(WIFEXITED(status) && WEXITSTATUS(status) == 0 ? 0 : 1);
	}
	ksft_test_result(pid > 0 && waitpid(pid, &status, 0) == pid &&
			 WIFEXITED(status) && WEXITSTATUS(status) == 0,
			 "PID namespace init retains inherited capability\n");
	if (getrlimit(RLIMIT_NOFILE, &original_limit))
		finish(1);
	small_limit = original_limit;
	small_limit.rlim_cur = 0;
	if (setrlimit(RLIMIT_NOFILE, &small_limit))
		finish(1);
	errno = 0;
	ok = create(&info) == -1 && errno == EMFILE;
	errno = 0;
	ok = (narrow(fd, AOK_RIGHT_INSPECT) == -1 && errno == EMFILE) && ok;
	if (setrlimit(RLIMIT_NOFILE, &original_limit))
		finish(1);
	ksft_test_result(ok && !inspect(fd, &info), "fd exhaustion leaves object intact\n");
	guard = mmap(NULL, page * 2, PROT_READ | PROT_WRITE,
		     MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (guard == MAP_FAILED || mprotect(guard + page, page, PROT_NONE))
		finish(1);
	*(uint64_t *)(guard + page - sizeof(uint64_t)) = sizeof(info);
	EXPECT_ERR(syscall(NR_AOK_CREATE, &args, guard + page - sizeof(uint64_t),
			   &create_result), EFAULT, "partial result fault");
	*(uint64_t *)(guard + page - sizeof(uint64_t)) = sizeof(args);
	EXPECT_ERR(syscall(NR_AOK_CREATE, guard + page - sizeof(uint64_t), &info,
			   &create_result), EFAULT, "partial args fault");
	munmap(guard, page * 2);
	raw_fd = open("/dev/null", O_RDWR);
	saved = raw_fd;
	close(raw_fd);
	for (i = 0; i < 100; i++)
		syscall(NR_AOK_CREATE, &args, NULL, &create_result);
	raw_fd = open("/dev/null", O_RDWR);
	ksft_test_result(raw_fd == saved, "failed creates do not leak fd slots\n");
	close(raw_fd);
	ok = true;
	for (i = 0; i < ARRAY_SIZE(threads); i++) {
		if (pthread_create(&threads[i], NULL, duplicate_worker, &fd)) {
			ok = false;
			break;
		}
		started++;
	}
	for (i = 0; i < started; i++) {
		void *result;

		if (pthread_join(threads[i], &result) || result)
			ok = false;
	}
	ksft_test_result(ok, "parallel duplicate inspect close (4000 cycles)\n");
	close(fd);
	ksft_test_result(!inspect(child_fd, &info) && info.aid == first.aid,
			 "object survives original handle close\n");
	close(child_fd);
	EXPECT_ERR(inspect(child_fd, &info), EBADF, "closed handle invalid");
	previous = first.aid;
	ok = true;
	for (i = 0; i < 100; i++) {
		fd = create(&info);
		if (fd < 0 || info.aid <= previous)
			ok = false;
		previous = info.aid;
		if (fd >= 0)
			close(fd);
	}
	ksft_test_result(ok, "AID monotonic and never reused after close\n");
	info.size = page + 1;
	EXPECT_ERR(syscall(NR_AOK_CREATE, &args, &info, &create_result), EINVAL, "oversized result buffer");
	fd = create(&info);
	ksft_test_result(fd >= 0, "create still works after failures\n");
	empty_fd = narrow(fd, 0);
	EXPECT_ERR(inspect(empty_fd, &info), EACCES, "zero rights handle denied");
	close(empty_fd);
	close(fd);
	fd = create(&info);
	ok = fd >= 0;
	for (i = 0; i < ARRAY_SIZE(closing); i++) {
		closing[i] = narrow(fd, AOK_RIGHT_INSPECT);
		if (closing[i] < 0)
			ok = false;
	}
	if (fd >= 0)
		close(fd);
	started = 0;
	for (i = 0; i < ARRAY_SIZE(closing); i++) {
		if (pthread_create(&threads[i], NULL, close_worker, &closing[i])) {
			ok = false;
			break;
		}
		started++;
	}
	for (i = 0; i < started; i++) {
		void *result;

		if (pthread_join(threads[i], &result) || result)
			ok = false;
	}
	for (i = started; i < ARRAY_SIZE(closing); i++)
		close(closing[i]);
	ksft_test_result(ok, "parallel close of final object references\n");
	close(root_fd);
	ksft_print_cnts();
	finish(ksft_get_fail_cnt() != 0 || ksft_test_num() != 44);
	return 0;
}
