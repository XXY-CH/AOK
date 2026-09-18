// SPDX-License-Identifier: GPL-2.0-only
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/aok.h>
#include <poll.h>
#include <stdint.h>
#include <stdlib.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <unistd.h>

#include "kselftest.h"

#define TEST_PLAN 37

_Static_assert(sizeof(struct aok_event_target) == 16, "event target ABI");

static void finish(void)
{
	int fail = ksft_get_fail_cnt() || ksft_test_num() != TEST_PLAN;

	printf("AOK_EVENTWAKE_TEST=%s\n", fail ? "fail" : "pass");
	fflush(stdout);
	if (getpid() == 1) {
		reboot(RB_POWER_OFF);
		for (;;)
			pause();
	}
	exit(fail);
}

#define EXPECT_ERR(expr, expected, name) do { \
	errno = 0; \
	long result = (expr); \
	ksft_test_result(result == -1 && errno == (expected), \
			 "%s (ret=%ld errno=%d)\n", name, result, errno); \
} while (0)

static int source(int root, unsigned int kind, uint64_t first)
{
	struct aok_event_source_attr attr = {
		.size = sizeof(attr), .parent_job_fd = root,
		.kind = kind, .first_ns = first,
	};
	struct aok_object_info info = { .size = sizeof(info) };

	return syscall(__NR_aok_event_source_create, &attr, &info);
}

static int ready(int fd, int timeout)
{
	struct pollfd p = { .fd = fd, .events = POLLIN };
	int ret = poll(&p, 1, timeout);

	return ret < 0 ? -1 : p.revents;
}

static uint32_t state(int fd)
{
	struct aok_aproc_status st = { .size = sizeof(st) };

	return syscall(__NR_aok_aproc_status, fd, &st) ? 0 : st.state;
}

static int ticking(uint64_t *ticks)
{
	uint64_t before = __atomic_load_n(ticks, __ATOMIC_RELAXED);
	int i;

	for (i = 0; i < 200; i++) {
		usleep(10000);
		if (__atomic_load_n(ticks, __ATOMIC_RELAXED) > before)
			return 1;
	}
	return 0;
}

static int stopped(int fd, uint64_t *ticks)
{
	uint64_t before;

	usleep(50000);
	before = __atomic_load_n(ticks, __ATOMIC_RELAXED);
	usleep(50000);
	return state(fd) == AOK_APROC_STATE_FROZEN &&
	       __atomic_load_n(ticks, __ATOMIC_RELAXED) == before;
}

int main(void)
{
	struct aok_event_record event;
	struct aok_event_target target = { .application_id = 77 };
	struct aok_aproc_create_args args = { .size = sizeof(args) };
	struct aok_aproc_create_result result = { .size = sizeof(result) };
	struct aok_object_info info = { .size = sizeof(info) };
	struct aok_budget budget = {
		.size = sizeof(budget), .cpu_usec_limit = UINT64_MAX,
		.memory_bytes_limit = UINT64_MAX, .token_hard_limit = 0,
	};
	struct aok_token_usage usage = {
		.size = sizeof(usage), .usage_id = 1, .usage_seq = 1,
		.input_tokens = 1,
	};
	uint64_t *ticks;
	int root, port, timer, copy, ro, wo, aproc, weak, signal_fd;
	unsigned int count, i;
	uint64_t last;
	bool ok;

	if (getpid() != 1)
		ksft_exit_skip("run as initial PID 1 in QEMU\n");
	mkdir("/proc", 0755);
	mkdir("/dev", 0755);
	mount("proc", "/proc", "proc", 0, NULL);
	mount("devtmpfs", "/dev", "devtmpfs", 0, NULL);
	setvbuf(stdout, NULL, _IONBF, 0);
	ksft_print_header();
	ksft_set_plan(TEST_PLAN);
	root = syscall(__NR_aok_root_cap_claim);
	port = source(root, AOK_EVENT_SOURCE_PORT, 0);
	ksft_test_result(root >= 0 && port >= 0, "port source creation\n");
	if (port < 0)
		finish();
	ksft_test_result(ready(port, 20) == 0, "empty port poll times out\n");
	ksft_test_result(!ioctl(port, AOK_EVENT_POST, 0), "port post accepted\n");
	ksft_test_result((ready(port, 1000) & POLLIN) &&
		read(port, &event, sizeof(event)) == sizeof(event) &&
		event.kind == AOK_EVENT_KIND_PORT && event.event_seq == 1,
		"port event is readable\n");
	ksft_test_result(ready(port, 0) == 0, "poll follows consumed cursor\n");
	copy = syscall(__NR_aok_handle_duplicate, port, AOK_RIGHT_READ);
	ksft_test_result(copy >= 0 && (ready(copy, 0) & POLLIN),
			 "fresh cursor polls retained event\n");
	EXPECT_ERR(syscall(__NR_aok_event_bind, port, 77, AOK_WAKE_MANUAL),
		   EBUSY, "cannot relabel unacked event");
	ksft_test_result(!syscall(__NR_aok_event_ack, port, event.event_id) &&
		ready(copy, 0) == 0, "ack removes readiness for all cursors\n");
	close(copy);
	ksft_test_result(!syscall(__NR_aok_event_bind, port, 77, AOK_WAKE_MANUAL),
			 "drained port can bind identity\n");
	ok = true;
	for (i = 0; i < AOK_EVENT_SOURCE_DEPTH; i++)
		ok &= ioctl(port, AOK_EVENT_POST, 0) == 0;
	ksft_test_result(ok, "port accepts bounded batch\n");
	EXPECT_ERR(ioctl(port, AOK_EVENT_POST, 0), EAGAIN, "full port refuses post");
	ok = read(port, &event, sizeof(event)) == sizeof(event) &&
	      !syscall(__NR_aok_event_ack, port, event.event_id) &&
	      !ioctl(port, AOK_EVENT_POST, 0);
	ksft_test_result(ok, "ack permits retry of refused post\n");
	count = 0;
	last = event.event_seq;
	ok = true;
	while (read(port, &event, sizeof(event)) == sizeof(event)) {
		ok &= event.application_id == 77 && event.event_seq == last + 1;
		last = event.event_seq;
		count++;
	}
	ksft_test_result(ok && count == AOK_EVENT_SOURCE_DEPTH && last == 66,
			 "refused post consumes no identity or sequence\n");
	ro = syscall(__NR_aok_handle_duplicate, port, AOK_RIGHT_READ);
	wo = syscall(__NR_aok_handle_duplicate, port, AOK_RIGHT_WRITE);
	EXPECT_ERR(ioctl(ro, AOK_EVENT_POST, 0), EACCES, "post requires WRITE");
	ksft_test_result(ready(wo, 0) == POLLERR, "poll requires READ\n");
	EXPECT_ERR(source(root, AOK_EVENT_SOURCE_PORT, 1), EINVAL,
		   "port rejects timer configuration");
	timer = source(root, AOK_EVENT_SOURCE_TIMER, 0);
	EXPECT_ERR(ioctl(timer, AOK_EVENT_POST, 0), EINVAL, "timer rejects port post");
	EXPECT_ERR(ioctl(port, 0xffffffffU, 0), ENOTTY, "unknown event ioctl");
	EXPECT_ERR(ioctl(port, AOK_EVENT_SET_TARGET, NULL), EFAULT, "target copy fault");
	close(timer);
	close(ro);
	close(wo);
	close(port);

	ticks = mmap(NULL, 4096, PROT_READ | PROT_WRITE,
		     MAP_SHARED | MAP_ANONYMOUS, -1, 0);
	if (ticks == MAP_FAILED)
		finish();
	args.parent_job_fd = root;
	aproc = syscall(__NR_aok_aproc_create, &args, &info, &result);
	if (aproc == 0) {
		for (;;) {
			__atomic_fetch_add(ticks, 1, __ATOMIC_RELAXED);
			usleep(1000);
		}
	}
	ksft_test_result(aproc >= 0 && ticking(ticks), "aproc task runs heartbeat\n");
	if (aproc < 0)
		finish();
	ksft_test_result(!syscall(__NR_aok_aproc_freeze, aproc) && stopped(aproc, ticks),
			 "freeze stops actual task execution\n");
	timer = source(root, AOK_EVENT_SOURCE_TIMER, 500000000);
	ksft_test_result(timer >= 0, "wake timer created\n");
	weak = syscall(__NR_aok_handle_duplicate, aproc, AOK_RIGHT_INSPECT);
	target.aproc_fd = weak;
	target.wake_policy = AOK_WAKE_ON_EVENT;
	EXPECT_ERR(ioctl(timer, AOK_EVENT_SET_TARGET, &target), EACCES,
		   "wake target requires SIGNAL");
	target.aproc_fd = root;
	EXPECT_ERR(ioctl(timer, AOK_EVENT_SET_TARGET, &target), EINVAL,
		   "wake target must be an aproc");
	ro = syscall(__NR_aok_handle_duplicate, timer, AOK_RIGHT_READ);
	target.aproc_fd = aproc;
	EXPECT_ERR(ioctl(ro, AOK_EVENT_SET_TARGET, &target), EACCES,
		   "target binding requires source WRITE");
	close(ro);
	signal_fd = syscall(__NR_aok_handle_duplicate, aproc, AOK_RIGHT_SIGNAL);
	target.aproc_fd = signal_fd;
	ksft_test_result(!ioctl(timer, AOK_EVENT_SET_TARGET, &target),
			 "authorized timer target bound\n");
	close(signal_fd);
	ksft_test_result((ready(timer, 2000) & POLLIN) && ticking(ticks) &&
		state(weak) == AOK_APROC_STATE_RUNNING,
		"timer poll wakes and task runs after target fd closes\n");
	ksft_test_result(read(timer, &event, sizeof(event)) == sizeof(event) &&
		event.application_id == 77 && event.kind == AOK_EVENT_KIND_TIMER,
		"wake leaves original event unacked\n");
	close(timer);

	port = source(root, AOK_EVENT_SOURCE_PORT, 0);
	target.aproc_fd = aproc;
	target.wake_policy = AOK_WAKE_MANUAL;
	ok = !ioctl(port, AOK_EVENT_SET_TARGET, &target) &&
	      !syscall(__NR_aok_aproc_freeze, aproc) &&
	      !ioctl(port, AOK_EVENT_POST, 0);
	ksft_test_result(ok && stopped(aproc, ticks), "manual policy keeps task frozen\n");
	read(port, &event, sizeof(event));
	syscall(__NR_aok_event_ack, port, event.event_id);
	target.wake_policy = AOK_WAKE_ON_QUIESCENT;
	ok = !ioctl(port, AOK_EVENT_SET_TARGET, &target) &&
	      !ioctl(port, AOK_EVENT_POST, 0);
	ksft_test_result(ok && ticking(ticks), "port event resumes quiescent task\n");
	read(port, &event, sizeof(event));
	syscall(__NR_aok_event_ack, port, event.event_id);
	target.aproc_fd = -1;
	target.wake_policy = AOK_WAKE_MANUAL;
	ok = !ioctl(port, AOK_EVENT_SET_TARGET, &target) &&
	      !syscall(__NR_aok_aproc_freeze, aproc) &&
	      !ioctl(port, AOK_EVENT_POST, 0);
	ksft_test_result(ok && stopped(aproc, ticks), "detach revokes automatic wake\n");
	read(port, &event, sizeof(event));
	syscall(__NR_aok_event_ack, port, event.event_id);
	syscall(__NR_aok_aproc_resume, aproc);
	target.aproc_fd = aproc;
	target.wake_policy = AOK_WAKE_ON_EVENT;
	ok = !ioctl(port, AOK_EVENT_SET_TARGET, &target);
	for (i = 0; ok && i < 50; i++) {
		ok = !syscall(__NR_aok_aproc_freeze, aproc) &&
		     !ioctl(port, AOK_EVENT_POST, 0) && ticking(ticks) &&
		     read(port, &event, sizeof(event)) == sizeof(event) &&
		     !syscall(__NR_aok_event_ack, port, event.event_id);
	}
	ksft_test_result(ok && i == 50, "repeated freeze and asynchronous wake stay live\n");
	ok = !syscall(__NR_aok_budget_set, aproc, &budget);
	errno = 0;
	ok &= syscall(__NR_aok_token_usage, aproc, &usage) == -1 && errno == EDQUOT;
	ksft_test_result(ok && stopped(aproc, ticks), "resource limit freezes task\n");
	ok = !ioctl(port, AOK_EVENT_POST, 0);
	ksft_test_result(ok && stopped(aproc, ticks), "automatic wake respects resource freeze\n");
	ksft_test_result(!syscall(__NR_aok_aproc_resume, aproc) && ticking(ticks),
			 "explicit authorized resume remains available\n");
	ok = !syscall(__NR_aok_aproc_abort, aproc);
	ioctl(port, AOK_EVENT_POST, 0);
	usleep(50000);
	ksft_test_result(ok && state(aproc) == AOK_APROC_STATE_FAILED,
			 "pending wake cannot resurrect aborted aproc\n");
	close(port);
	close(weak);
	close(result.pidfd);
	while (waitpid(-1, NULL, WNOHANG) > 0)
		;
	ksft_test_result(!syscall(__NR_aok_aproc_reap, aproc), "wake target reaps normally\n");
	close(aproc);
	close(root);
	munmap(ticks, 4096);
	finish();
	return 0;
}
