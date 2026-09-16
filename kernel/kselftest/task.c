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
#define NR_AOK_SPAWN __NR_aok_aproc_spawn
#define NR_AOK_ATTACH __NR_aok_aproc_attach
#define NR_AOK_FREEZE __NR_aok_aproc_freeze
#define NR_AOK_RESUME __NR_aok_aproc_resume
#define NR_AOK_ABORT __NR_aok_aproc_abort
#define NR_AOK_REAP __NR_aok_aproc_reap
#define NR_AOK_STATUS __NR_aok_aproc_status

_Static_assert(sizeof(struct aok_aproc_create_args) == 40, "create args");
_Static_assert(sizeof(struct aok_aproc_create_result) == 40, "create result");
_Static_assert(sizeof(struct aok_task_spawn_attr) == 40, "spawn attr");
_Static_assert(sizeof(struct aok_aproc_status) == 88, "status");
_Static_assert(sizeof(struct aok_aproc_event) == 64, "event");

#define AOK_TEST_PLAN 64

static int root_fd = -1;
static struct aok_aproc_create_args create_args = { .size = 40, };
static struct aok_task_spawn_attr spawn_attr = { .size = 40, };

#define EXPECT_ERR(expr, error, name) do { \
	errno = 0; \
	long result = (expr); \
	int saved_errno = errno; \
	ksft_test_result(result == -1 && saved_errno == (error), \
			 "%s (ret=%ld errno=%d)\n", name, result, saved_errno); \
	if (result >= 0 && result != 0) close(result); \
} while (0)

static void drain_zombies(void)
{
	while (waitpid(-1, NULL, WNOHANG) > 0)
		;
}

/* First tasks that must keep their aproc RUNNING sit in pause() until the
 * test kills them through the pidfd; ephemeral tasks exit immediately. */
static int create_living(struct aok_object_info *info,
			 struct aok_aproc_create_result *result)
{
	long r;

	create_args.parent_job_fd = root_fd;
	info->size = sizeof(*info);
	result->size = sizeof(*result);
	r = syscall(NR_AOK_CREATE, &create_args, info, result);
	if (r == 0) {
		for (;;)
			pause();
	}
	return r;
}

static int create_ephemeral(struct aok_object_info *info,
			    struct aok_aproc_create_result *result)
{
	long r;

	create_args.parent_job_fd = root_fd;
	info->size = sizeof(*info);
	result->size = sizeof(*result);
	r = syscall(NR_AOK_CREATE, &create_args, info, result);
	if (r == 0)
		_exit(0);
	return r;
}

static int spawn_living(int fd)
{
	long r = syscall(NR_AOK_SPAWN, fd, &spawn_attr);

	if (r == 0) {
		for (;;)
			pause();
	}
	return r;
}

static int spawn_ephemeral(int fd)
{
	long r = syscall(NR_AOK_SPAWN, fd, &spawn_attr);

	if (r == 0)
		_exit(0);
	return r;
}

static int inspect(int fd, struct aok_object_info *info)
{
	info->size = sizeof(*info);
	return syscall(NR_AOK_INSPECT, fd, info);
}

static int status(int fd, struct aok_aproc_status *st)
{
	st->size = sizeof(*st);
	return syscall(NR_AOK_STATUS, fd, st);
}

static int attach(int fd, int pidfd)
{
	return syscall(NR_AOK_ATTACH, fd, pidfd);
}

static int narrow(int fd, uint64_t rights)
{
	return syscall(NR_AOK_DUPLICATE, fd, rights);
}

static int kill_pidfd(int pidfd)
{
	siginfo_t si;

	if (syscall(__NR_pidfd_send_signal, pidfd, SIGKILL, NULL, 0))
		return -1;
	memset(&si, 0, sizeof(si));
	return waitid(P_PIDFD, pidfd, &si, WEXITED);
}

static int wait_pidfd_killed(int pidfd)
{
	siginfo_t si;

	memset(&si, 0, sizeof(si));
	if (waitid(P_PIDFD, pidfd, &si, WEXITED))
		return -1;
	return si.si_code == CLD_KILLED && si.si_status == SIGKILL ? 0 : -1;
}

static void finish(int fail)
{
	printf("AOK_TASK_TEST=%s\n", fail ? "fail" : "pass");
	fflush(stdout);
	if (getpid() == 1) {
		reboot(RB_POWER_OFF);
		for (;;)
			pause();
	}
	exit(fail);
}

struct stress_ctx {
	int fd;
	uint64_t aid;
};

/* Concurrent callers may lose races with terminal transitions; only these
 * errors (and success) are acceptable for every stress call. */
static int lifecycle_err_ok(int err)
{
	return !err || err == EINVAL || err == EACCES || err == EBUSY;
}

static void *stress_lifecycle(void *arg)
{
	struct stress_ctx *ctx = arg;
	int i;

	for (i = 0; i < 200; i++) {
		errno = 0;
		if (syscall(NR_AOK_FREEZE, ctx->fd) == -1 && !lifecycle_err_ok(errno))
			return (void *)1;
		usleep(2000);
		errno = 0;
		if (syscall(NR_AOK_RESUME, ctx->fd) == -1 && !lifecycle_err_ok(errno))
			return (void *)1;
	}
	return NULL;
}

static void *stress_reap(void *arg)
{
	struct stress_ctx *ctx = arg;
	int i;

	for (i = 0; i < 200; i++) {
		errno = 0;
		if (syscall(NR_AOK_REAP, ctx->fd) == -1 && !lifecycle_err_ok(errno))
			return (void *)1;
	}
	return NULL;
}

static void *stress_status(void *arg)
{
	struct stress_ctx *ctx = arg;
	struct aok_aproc_status st;
	int i;

	for (i = 0; i < 200; i++) {
		if (status(ctx->fd, &st) || st.aid != ctx->aid)
			return (void *)1;
	}
	return NULL;
}

int main(int argc, char **argv)
{
	struct aok_aproc_create_result result = {};
	struct aok_object_info first = {}, info = {};
	struct aok_aproc_status st = {};
	struct aok_aproc_event ev;
	struct rlimit original_limit, small_limit;
	uint64_t *counter;
	int fd, fd2, fd3, pidfd, pidfd2, attach_fd, worker_fd;
	int raw_fd, saved, status2, spawned;
	unsigned int i;
	long page = sysconf(_SC_PAGESIZE);
	char *guard, buf[16];
	uint64_t previous, aid_mid;
	ssize_t n;
	pid_t pid;
	bool ok;
	pthread_t threads[3];
	unsigned int started = 0;
	void *(*fns[3])(void *) = { stress_lifecycle, stress_reap, stress_status };
	struct stress_ctx ctx;
	int pipefd[2], cross_nr = -1;

	if (getpid() == 1) {
		mkdir("/proc", 0755);
		mkdir("/dev", 0755);
		mount("proc", "/proc", "proc", 0, NULL);
		mount("devtmpfs", "/dev", "devtmpfs", 0, NULL);
	}
	setvbuf(stdout, NULL, _IONBF, 0);
	ksft_print_header();
	if (argc == 2 && !strcmp(argv[1], "--expect-disabled")) {
		ksft_set_plan(11);
		EXPECT_ERR(syscall(NR_AOK_ROOT_CLAIM), ENOSYS, "disabled claim");
		EXPECT_ERR(syscall(NR_AOK_CREATE, &create_args, &first, &result), ENOSYS, "disabled create");
		EXPECT_ERR(syscall(NR_AOK_INSPECT, -1, &first), ENOSYS, "disabled inspect");
		EXPECT_ERR(syscall(NR_AOK_DUPLICATE, -1, 0), ENOSYS, "disabled duplicate");
		EXPECT_ERR(syscall(NR_AOK_SPAWN, -1, &spawn_attr), ENOSYS, "disabled spawn");
		EXPECT_ERR(syscall(NR_AOK_ATTACH, -1, -1), ENOSYS, "disabled attach");
		EXPECT_ERR(syscall(NR_AOK_FREEZE, -1), ENOSYS, "disabled freeze");
		EXPECT_ERR(syscall(NR_AOK_RESUME, -1), ENOSYS, "disabled resume");
		EXPECT_ERR(syscall(NR_AOK_ABORT, -1), ENOSYS, "disabled abort");
		EXPECT_ERR(syscall(NR_AOK_REAP, -1), ENOSYS, "disabled reap");
		EXPECT_ERR(syscall(NR_AOK_STATUS, -1, &st), ENOSYS, "disabled status");
		ksft_print_cnts();
		finish(ksft_get_fail_cnt() != 0);
	}
	if (getpid() != 1)
		ksft_exit_skip("run as initial PID 1 in the QEMU test initramfs\n");
	ksft_set_plan(AOK_TEST_PLAN);

	root_fd = syscall(NR_AOK_ROOT_CLAIM);
	ksft_test_result(root_fd >= 0, "PID1 claims root capability\n");

	fd = create_living(&first, &result);
	ksft_test_result(fd >= 0 && result.pidfd >= 0 && result.pidfd != fd,
			 "create returns aproc fd and first task pidfd\n");
	if (fd < 0 || result.pidfd < 0)
		finish(1);
	ksft_test_result(fcntl(fd, F_GETFD) == FD_CLOEXEC &&
			 fcntl(result.pidfd, F_GETFD) == FD_CLOEXEC,
			 "create descriptors are CLOEXEC\n");
	ksft_test_result(first.aid && first.type == AOK_OBJECT_APROC &&
			 first.state == AOK_APROC_STATE_RUNNING &&
			 first.rights == AOK_RIGHTS_APROC_INITIAL,
			 "created aproc is RUNNING with full initial rights\n");
	ok = !status(fd, &st) && st.aid == first.aid &&
	     st.state == AOK_APROC_STATE_RUNNING && st.task_count == 1 &&
	     st.task_total == 1 && st.rights_ceiling == AOK_RIGHTS_APROC_INITIAL;
	ksft_test_result(ok, "status sees task count and state\n");

	ok = read(fd, &ev, sizeof(ev)) == sizeof(ev) &&
	     ev.kind == AOK_EVENT_TASK_JOINED && ev.aid == first.aid &&
	     ev.task_koid > 0 && ev.event_seq == 1 && ev.size == sizeof(ev);
	ksft_test_result(ok, "first event is the ordered task join\n");
	pidfd2 = spawn_living(fd);
	ksft_test_result(pidfd2 >= 0 && pidfd2 != result.pidfd && pidfd2 != fd,
			 "spawn returns a distinct pidfd\n");
	ok = read(fd, &ev, sizeof(ev)) == sizeof(ev) &&
	     ev.kind == AOK_EVENT_TASK_JOINED && ev.event_seq == 2 &&
	     ev.task_koid > 0;
	ksft_test_result(ok, "spawn appends a join event\n");
	EXPECT_ERR(read(fd, &ev, sizeof(ev) - 1), EINVAL, "short event buffer");

	/* Inheritance: a spawned member task forks a plain child; the child
	 * inherits the membership without any syscall opting in. */
	pid = fork();
	if (pid == 0) {
		int pfd = syscall(NR_AOK_SPAWN, fd, &spawn_attr);

		if (pfd == 0) {
			pid_t nested = fork();

			if (nested == 0)
				_exit(42);	/* plain fork: inherited */
			if (nested < 0 || waitpid(nested, &status2, 0) != nested)
				_exit(1);
			_exit(WIFEXITED(status2) && WEXITSTATUS(status2) == 42 ? 7 : 1);
		}
		if (pfd < 0)
			_exit(1);
		close(pfd);
		for (;;) {
			pid_t w = waitpid(-1, &status2, 0);

			if (w < 0)
				_exit(1);
			if (WIFEXITED(status2) && WEXITSTATUS(status2) == 7)
				_exit(7);
		}
	}
	while ((pid = waitpid(pid, &status2, 0)) < 0 && errno == EINTR)
		;
	ksft_test_result(pid > 0 && WIFEXITED(status2) && WEXITSTATUS(status2) == 7,
			 "plain fork child inherits aproc membership\n");
	ok = !status(fd, &st) && st.task_total == 4;
	ksft_test_result(ok, "task total counts inherited fork\n");
	drain_zombies();

	/* Attach: rights, pidfd kind, membership, self and namespace rules. */
	attach_fd = narrow(fd, AOK_RIGHT_INSPECT);
	EXPECT_ERR(attach(attach_fd, 0), EACCES, "attach right required");
	close(attach_fd);
	EXPECT_ERR(attach(fd, -1), EBADF, "attach invalid pidfd");
	raw_fd = open("/dev/null", O_RDWR);
	EXPECT_ERR(attach(fd, raw_fd), EINVAL, "attach non-pidfd fd");
	EXPECT_ERR(attach(fd, fd), EINVAL, "attach non-pidfd capability fd");
	close(raw_fd);
	attach_fd = syscall(__NR_pidfd_open, getpid(), 0);
	EXPECT_ERR(attach(fd, attach_fd), EINVAL, "attach self rejected");
	close(attach_fd);
	pid = fork();
	if (pid == 0) {
		for (;;)
			pause();
	}
	attach_fd = syscall(__NR_pidfd_open, pid, 0);
	ok = attach_fd >= 0 && !attach(fd, attach_fd);
	ksft_test_result(ok, "attach joins an external task by pidfd\n");
	ok = !status(fd, &st) && st.task_total == 5 && st.task_count == 3;
	ksft_test_result(ok, "status counts the attached task\n");
	{
		int second = syscall(__NR_pidfd_open, pid, 0);

		EXPECT_ERR(attach(fd, second), EBUSY, "attach existing member rejected");
		close(second);
	}
	kill(pid, SIGKILL);
	waitpid(pid, NULL, 0);
	close(attach_fd);

	/* Cross namespace target: pidfd resolves, attach is still denied. */
	if (pipe(pipefd))
		finish(1);
	pid = fork();
	if (pid == 0) {
		pid_t nested;

		close(pipefd[0]);
		if (unshare(CLONE_NEWPID))
			_exit(1);
		nested = fork();
		if (nested == 0) {
			for (;;)
				pause();
		}
		if (nested < 0)
			_exit(1);
		snprintf(buf, sizeof(buf), "%d", nested);
		if (write(pipefd[1], buf, strlen(buf)) != (ssize_t)strlen(buf))
			_exit(1);
		for (;;)
			pause();
	}
	close(pipefd[1]);
	n = read(pipefd[0], buf, sizeof(buf) - 1);
	close(pipefd[0]);
	if (n <= 0)
		finish(1);
	buf[n] = 0;
	cross_nr = atoi(buf);
	attach_fd = syscall(__NR_pidfd_open, cross_nr, 0);
	errno = 0;
	ok = attach_fd >= 0 && attach(fd, attach_fd) == -1 && errno == EPERM;
	ksft_test_result(ok, "cross namespace attach denied\n");
	if (attach_fd >= 0)
		close(attach_fd);
	kill(pid, SIGKILL);
	waitpid(pid, NULL, 0);
	kill(cross_nr, SIGKILL);
	drain_zombies();

	/* Task death and pidfd close keep the aproc inspectable; the aproc
	 * reaches EXITED only after the last member is gone. */
	ok = !kill_pidfd(result.pidfd) && !kill_pidfd(pidfd2);
	close(result.pidfd);
	close(pidfd2);
	ok = ok && !inspect(fd, &info) && info.aid == first.aid;
	ksft_test_result(ok, "task exit and pidfd close keep aproc inspectable\n");
	ok = !status(fd, &st) && st.task_total == 5 && st.task_count == 0 &&
	     st.state == AOK_APROC_STATE_EXITED;
	ksft_test_result(ok, "aproc reaches EXITED after last task leaves\n");
	ok = true;
	i = 0;
	previous = 2;
	while (ok) {
		ssize_t r = read(fd, &ev, sizeof(ev));

		if (r == 0)
			break;
		if (r != sizeof(ev) || ev.event_seq != previous + 1 ||
		    ev.aid != first.aid)
			ok = false;
		previous = ev.event_seq;
		i++;
	}
	ksft_test_result(ok && i == 8, "eight further ordered events drained\n");
	ok = ev.kind == AOK_EVENT_TASK_EXITED && ev.state == AOK_APROC_STATE_EXITED;
	ksft_test_result(ok, "last task exit carries the EXITED state\n");

	/* Lifecycle on a fresh aproc: freeze, resume, abort, reap. */
	fd2 = create_living(&info, &result);
	if (fd2 < 0 || result.pidfd < 0)
		finish(1);
	EXPECT_ERR(syscall(NR_AOK_REAP, fd2), EBUSY, "reap running aproc");
	ok = !syscall(NR_AOK_FREEZE, fd2) && !inspect(fd2, &info) &&
	     info.state == AOK_APROC_STATE_FROZEN;
	ksft_test_result(ok, "freeze moves aproc to FROZEN\n");
	EXPECT_ERR(syscall(NR_AOK_REAP, fd2), EBUSY, "reap frozen aproc");
	ok = syscall(NR_AOK_RESUME, fd2) == AOK_RESUME_RECOVERY_NONE &&
	     !status(fd2, &st) && st.state == AOK_APROC_STATE_RUNNING &&
	     st.recovery == AOK_RESUME_RECOVERY_NONE;
	ksft_test_result(ok, "resume returns recovery none without checkpoint\n");

	/* The freezer must actually stop a counting member task. */
	counter = mmap(NULL, page, PROT_READ | PROT_WRITE,
		       MAP_SHARED | MAP_ANONYMOUS, -1, 0);
	if (counter == MAP_FAILED)
		finish(1);
	*counter = 0;
	pid = fork();
	if (pid == 0) {
		for (;;) {
			(*counter)++;
			usleep(1000);
		}
	}
	worker_fd = syscall(__NR_pidfd_open, pid, 0);
	if (worker_fd < 0 || attach(fd2, worker_fd))
		finish(1);
	usleep(150000);
	ok = !syscall(NR_AOK_FREEZE, fd2);
	usleep(250000);
	previous = *counter;
	usleep(300000);
	ok = ok && *counter == previous;
	ksft_test_result(ok, "frozen aproc tasks stop running\n");
	ok = syscall(NR_AOK_RESUME, fd2) == AOK_RESUME_RECOVERY_NONE;
	usleep(300000);
	ok = ok && *counter > previous;
	ksft_test_result(ok, "resume restarts the tasks\n");
	kill(pid, SIGKILL);
	waitpid(pid, NULL, 0);
	close(worker_fd);

	ok = !syscall(NR_AOK_ABORT, fd2) && !status(fd2, &st) &&
	     st.state == AOK_APROC_STATE_FAILED;
	ksft_test_result(ok, "abort moves aproc to FAILED\n");
	ok = !wait_pidfd_killed(result.pidfd);
	ksft_test_result(ok, "abort terminates the bound task\n");
	ok = !syscall(NR_AOK_ABORT, fd2);
	ksft_test_result(ok, "abort is idempotent\n");
	ok = !syscall(NR_AOK_REAP, fd2) && !status(fd2, &st) &&
	     st.state == AOK_APROC_STATE_REAPED;
	ksft_test_result(ok, "reap finalizes the failed aproc\n");
	ok = !syscall(NR_AOK_REAP, fd2) && !inspect(fd2, &info) &&
	     info.aid == st.aid;
	ksft_test_result(ok, "reap is idempotent and inspectable\n");
	EXPECT_ERR(syscall(NR_AOK_ABORT, fd2), EINVAL, "abort reaped aproc");
	EXPECT_ERR(syscall(NR_AOK_SPAWN, fd2, &spawn_attr), EINVAL, "spawn reaped aproc");
	close(fd2);
	close(result.pidfd);
	drain_zombies();

	/* Structural rules for the new records. */
	EXPECT_ERR(syscall(NR_AOK_SPAWN, fd, NULL), EFAULT, "null spawn attr");
	spawn_attr.size--;
	EXPECT_ERR(spawn_ephemeral(fd), EINVAL, "short spawn attr");
	spawn_attr.size = page + 1;
	EXPECT_ERR(spawn_ephemeral(fd), EINVAL, "oversized spawn attr");
	spawn_attr.size = sizeof(spawn_attr);
	spawn_attr.flags = 1;
	EXPECT_ERR(spawn_ephemeral(fd), EINVAL, "unknown spawn flags");
	spawn_attr.flags = 0;
	spawn_attr.reserved2[2] = 1;
	EXPECT_ERR(spawn_ephemeral(fd), EINVAL, "spawn reserved field");
	spawn_attr.reserved2[2] = 0;
	EXPECT_ERR(syscall(NR_AOK_STATUS, fd, NULL), EFAULT, "null status");
	st.size = sizeof(st) - 1;
	EXPECT_ERR(syscall(NR_AOK_STATUS, fd, &st), EINVAL, "short status buffer");
	memset(&st, 0xff, sizeof(st));
	st.size = page + 1;
	EXPECT_ERR(syscall(NR_AOK_STATUS, fd, &st), EINVAL, "oversized status buffer");
	{
		struct {
			struct aok_aproc_status status;
			uint64_t tail;
		} extended;

		memset(&extended, 0xff, sizeof(extended));
		extended.status.size = sizeof(extended);
		ok = !syscall(NR_AOK_STATUS, fd, &extended) && extended.tail == 0;
		ksft_test_result(ok, "extended status zero filled\n");
	}
	{
		int evfd = narrow(fd, AOK_RIGHT_INSPECT);

		ok = evfd >= 0 && read(evfd, &ev, sizeof(ev)) == sizeof(ev) &&
		     ev.size == sizeof(ev) && !ev.reserved[0] &&
		     !ev.reserved[1] && !ev.reserved[2];
		ksft_test_result(ok, "event record reserved fields zero\n");
		close(evfd);
	}
	EXPECT_ERR(syscall(NR_AOK_CREATE, &create_args, &first, NULL), EFAULT,
		   "null create result");
	{
		struct {
			struct aok_aproc_create_result result;
			uint64_t tail;
		} extended;

		struct aok_object_info local_info = { .size = sizeof(local_info), };

		memset(&extended, 0xff, sizeof(extended));
		extended.result.size = sizeof(extended);
		create_args.parent_job_fd = root_fd;
		fd2 = syscall(NR_AOK_CREATE, &create_args, &local_info,
			      &extended.result);
		if (fd2 == 0)
			_exit(0);
		ok = fd2 >= 0 && extended.tail == 0 &&
		     local_info.rights == AOK_RIGHTS_APROC_INITIAL;
		ksft_test_result(ok, "zero extended create result accepted\n");
		if (fd2 >= 0) {
			close(extended.result.pidfd);
			close(fd2);
		}
	}
	drain_zombies();

	/* Result copyout fault: descriptors roll back, no fd slot leaks. */
	guard = mmap(NULL, page * 2, PROT_READ | PROT_WRITE,
		     MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (guard == MAP_FAILED || mprotect(guard + page, page, PROT_NONE))
		finish(1);
	raw_fd = open("/dev/null", O_RDWR);
	saved = raw_fd;
	close(raw_fd);
	*(uint64_t *)(guard + page - sizeof(uint64_t)) = sizeof(result);
	create_args.parent_job_fd = root_fd;
	result.size = sizeof(result);
	{
		struct aok_object_info local_info = { .size = sizeof(local_info), };
		long r = syscall(NR_AOK_CREATE, &create_args, &local_info,
				 guard + page - sizeof(uint64_t));

		if (r == 0)
			_exit(0);	/* forked child of the faulting create */
		ksft_test_result(r == -1 && errno == EFAULT,
				 "partial result fault (ret=%ld errno=%d)\n", r, errno);
	}
	raw_fd = open("/dev/null", O_RDWR);
	ksft_test_result(raw_fd == saved, "failed create leaves fd slots clean\n");
	close(raw_fd);
	munmap(guard, page * 2);
	drain_zombies();

	/* fd exhaustion fails before any task or object is published. The
	 * spawn probe needs a RUNNING aproc; fd3 doubles for the ring test. */
	fd3 = create_living(&info, &result);
	if (fd3 < 0)
		finish(1);
	if (getrlimit(RLIMIT_NOFILE, &original_limit))
		finish(1);
	small_limit = original_limit;
	small_limit.rlim_cur = 0;
	if (setrlimit(RLIMIT_NOFILE, &small_limit))
		finish(1);
	{
		struct aok_object_info throwaway = { .size = sizeof(throwaway), };
		struct aok_aproc_create_result toss = { .size = sizeof(toss), };

		errno = 0;
		ok = syscall(NR_AOK_CREATE, &create_args, &throwaway, &toss) == -1 &&
		     errno == EMFILE;
		errno = 0;
		ok = (syscall(NR_AOK_SPAWN, fd3, &spawn_attr) == -1 &&
		      errno == EMFILE) && ok;
	}
	if (setrlimit(RLIMIT_NOFILE, &original_limit))
		finish(1);
	ksft_test_result(ok && !inspect(fd3, &info), "fd exhaustion fails cleanly\n");

	/* Event ring overflow drops oldest and reports it. */
	spawned = 0;
	for (i = 0; i < 140; i++) {
		pidfd = spawn_ephemeral(fd3);
		if (pidfd < 0)
			break;
		close(pidfd);
		spawned++;
	}
	drain_zombies();
	ok = spawned == 140 && !status(fd3, &st) && st.events_dropped > 0 &&
	     st.task_total == 141;
	ksft_test_result(ok, "overflowed ring reports dropped events\n");
	EXPECT_ERR(read(fd3, &ev, sizeof(ev)), EOVERFLOW, "stale cursor overflows");
	syscall(NR_AOK_ABORT, fd3);
	syscall(NR_AOK_REAP, fd3);
	close(fd3);
	drain_zombies();

	/* Rights gate the lifecycle calls before any state check. */
	attach_fd = narrow(fd, AOK_RIGHT_INSPECT);
	EXPECT_ERR(syscall(NR_AOK_FREEZE, attach_fd), EACCES, "freeze right required");
	EXPECT_ERR(syscall(NR_AOK_REAP, attach_fd), EACCES, "reap right required");
	EXPECT_ERR(syscall(NR_AOK_SPAWN, attach_fd, &spawn_attr), EACCES, "spawn right required");
	EXPECT_ERR(syscall(NR_AOK_ABORT, attach_fd), EACCES, "abort right required");
	close(attach_fd);
	attach_fd = narrow(fd, 0);
	EXPECT_ERR(syscall(NR_AOK_STATUS, attach_fd, &st), EACCES, "status right required");
	EXPECT_ERR(read(attach_fd, &ev, sizeof(ev)), EACCES, "read right required");
	close(attach_fd);

	/* Parallel freeze/resume, reap and status must stay consistent. */
	fd2 = create_living(&info, &result);
	ctx.fd = fd2;
	ctx.aid = info.aid;
	ok = fd2 >= 0;
	for (i = 0; i < 3 && ok; i++) {
		if (pthread_create(&threads[i], NULL, fns[i], &ctx)) {
			ok = false;
			break;
		}
		started++;
	}
	for (i = 0; i < started; i++) {
		void *ret;

		if (pthread_join(threads[i], &ret) || ret)
			ok = false;
	}
	ksft_test_result(ok, "parallel lifecycle calls stay consistent\n");
	ok = fd2 >= 0 && !syscall(NR_AOK_ABORT, fd2) &&
	     !syscall(NR_AOK_REAP, fd2) && !status(fd2, &st) &&
	     st.state == AOK_APROC_STATE_REAPED;
	ksft_test_result(ok, "aborted aproc converges to REAPED\n");
	if (fd2 >= 0) {
		close(result.pidfd);
		close(fd2);
	}
	drain_zombies();

	/* AIDs stay monotonic across full lifecycles. */
	fd2 = create_ephemeral(&info, &result);
	ok = fd2 >= 0 && info.aid > first.aid;
	if (ok) {
		aid_mid = info.aid;
		syscall(NR_AOK_ABORT, fd2);
		syscall(NR_AOK_REAP, fd2);
		close(fd2);
		drain_zombies();
		fd2 = create_ephemeral(&info, &result);
		ok = fd2 >= 0 && info.aid > aid_mid;
		if (fd2 >= 0) {
			close(result.pidfd);
			close(fd2);
		}
	}
	ksft_test_result(ok, "AIDs monotonic across reap cycles\n");
	drain_zombies();

	/* A fresh handle drains the primary aproc to zero exactly once. */
	attach_fd = narrow(fd, AOK_RIGHT_INSPECT);
	i = 0;
	ok = attach_fd >= 0;
	while (ok) {
		ssize_t r = read(attach_fd, &ev, sizeof(ev));

		if (r == 0)
			break;
		if (r != sizeof(ev) || ev.aid != first.aid)
			ok = false;
		i++;
	}
	ksft_test_result(ok && i == 10, "fresh cursor drains ten events\n");
	close(attach_fd);
	close(fd);
	close(root_fd);
	ksft_print_cnts();
	finish(ksft_get_fail_cnt() != 0 || ksft_test_num() != AOK_TEST_PLAN);
	return 0;
}
