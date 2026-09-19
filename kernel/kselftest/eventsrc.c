// SPDX-License-Identifier: GPL-2.0-only
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/aok.h>
#include <signal.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/mount.h>
#include <sys/reboot.h>
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
#define NR_AOK_SOURCE_CREATE __NR_aok_event_source_create
#define NR_AOK_EVENT_BIND __NR_aok_event_bind
#define NR_AOK_EVENT_ACK __NR_aok_event_ack

_Static_assert(sizeof(struct aok_event_source_attr) == 56, "source attr");
_Static_assert(sizeof(struct aok_event_record) == 72, "event record");

#define AOK_EVENTSRC_TEST_PLAN 38

#define AOK_RIGHTS_SOURCE_INITIAL \
	(AOK_RIGHT_INSPECT | AOK_RIGHT_DUPLICATE | AOK_RIGHT_READ | \
	 AOK_RIGHT_WRITE)

static int root_fd = -1;
static struct aok_aproc_create_args create_args = { .size = 40, };
static struct aok_object_info info = {};

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

static int make_source(struct aok_event_source_attr *attr)
{
	attr->size = sizeof(*attr);
	attr->parent_job_fd = root_fd;
	info.size = sizeof(info);
	return syscall(NR_AOK_SOURCE_CREATE, attr, &info);
}

static int read_event(int fd, struct aok_event_record *rec)
{
	memset(rec, 0, sizeof(*rec));
	return read(fd, rec, sizeof(*rec));
}

static void finish(int fail)
{
	printf("AOK_EVENTSRC_TEST=%s\n", fail ? "fail" : "pass");
	fflush(stdout);
	if (getpid() == 1) {
		reboot(RB_POWER_OFF);
		for (;;)
			pause();
	}
	exit(fail);
}

int main(int argc, char **argv)
{
	struct aok_event_source_attr attr = {};
	struct aok_aproc_create_result aproc_result = {};
	struct aok_event_record ev;
	long page = sysconf(_SC_PAGESIZE);
	char *guard;
	int fd, fd2, fd3, ro_fd, wo_fd, raw_fd;
	unsigned int seen;
	int flush_coalesced;
	uint64_t seq_prev, first_id = 0, replay_id;
	bool ok;

	if (getpid() == 1) {
		mkdir("/proc", 0755);
		mkdir("/dev", 0755);
		mount("proc", "/proc", "proc", 0, NULL);
		mount("devtmpfs", "/dev", "devtmpfs", 0, NULL);
	}
	setvbuf(stdout, NULL, _IONBF, 0);
	ksft_print_header();
	if (argc == 2 && !strcmp(argv[1], "--expect-disabled")) {
		ksft_set_plan(6);
		EXPECT_ERR(syscall(NR_AOK_ROOT_CLAIM), ENOSYS, "disabled claim");
		EXPECT_ERR(syscall(NR_AOK_CREATE, &create_args, &info, &aproc_result), ENOSYS, "disabled create");
		EXPECT_ERR(syscall(NR_AOK_INSPECT, -1, &info), ENOSYS, "disabled inspect");
		EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), ENOSYS, "disabled source create");
		EXPECT_ERR(syscall(NR_AOK_EVENT_BIND, -1, 1, 1), ENOSYS, "disabled bind");
		EXPECT_ERR(syscall(NR_AOK_EVENT_ACK, -1, 1), ENOSYS, "disabled ack");
		ksft_print_cnts();
		finish(ksft_get_fail_cnt() != 0);
	}
	if (getpid() != 1)
		ksft_exit_skip("run as initial PID 1 in the QEMU test initramfs\n");
	ksft_set_plan(AOK_EVENTSRC_TEST_PLAN);

	root_fd = syscall(NR_AOK_ROOT_CLAIM);
	ksft_test_result(root_fd >= 0, "PID1 claims root capability\n");

	/* One-shot timer: no event before expiry, exactly one after. */
	attr.kind = AOK_EVENT_SOURCE_TIMER;
	attr.flags = AOK_TIMER_ONESHOT;
	attr.first_ns = 80000000;
	fd = make_source(&attr);
	ksft_test_result(fd >= 0 && info.type == AOK_OBJECT_EVENT_SOURCE &&
			 info.aid > 0 && info.rights == AOK_RIGHTS_SOURCE_INITIAL,
			 "timer source created with initial rights\n");
	if (fd < 0)
		finish(1);
	ksft_test_result(fcntl(fd, F_GETFD) == FD_CLOEXEC, "source fd CLOEXEC\n");
	usleep(10000);
	ksft_test_result(read_event(fd, &ev) == 0, "no event before expiry\n");
	usleep(120000);
	ok = read_event(fd, &ev) == sizeof(ev) &&
	     ev.kind == AOK_EVENT_KIND_TIMER && ev.event_seq == 1 &&
	     ev.event_id == 1 && ev.coalesced == 1 &&
	     ev.application_id == 0 && ev.source_koid == info.aid &&
	     ev.size == sizeof(ev) && !ev.reserved[0] && !ev.reserved[1] &&
	     !ev.data;
	ksft_test_result(ok, "timer expiry produces one event\n");
	EXPECT_ERR(read(fd, &ev, sizeof(ev) - 1), EINVAL, "short event buffer");
	/* A fresh cursor sees the unacked event again; a faulting buffer
	 * loses that replay while the queue keeps the record. */
	guard = mmap(NULL, page * 2, PROT_READ | PROT_WRITE,
		     MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
	if (guard == MAP_FAILED || mprotect(guard + page, page, PROT_NONE))
		finish(1);
	fd3 = syscall(NR_AOK_DUPLICATE, fd, AOK_RIGHTS_SOURCE_INITIAL);
	if (fd3 < 0)
		finish(1);
	EXPECT_ERR(read(fd3, guard + page - sizeof(uint64_t), sizeof(ev)),
		   EFAULT, "partial event fault");
	close(fd3);
	munmap(guard, page * 2);
	ksft_test_result(read_event(fd, &ev) == 0, "one-shot stays drained\n");

	/* Ack: settles, is idempotent, and rejects unknown ids. */
	ok = !syscall(NR_AOK_EVENT_ACK, fd, 1);
	ksft_test_result(ok, "ack settles a delivered event\n");
	ok = !syscall(NR_AOK_EVENT_ACK, fd, 1);
	ksft_test_result(ok, "repeated ack is idempotent\n");
	EXPECT_ERR(syscall(NR_AOK_EVENT_ACK, fd, 999), EINVAL,
		   "ack unknown event id");
	EXPECT_ERR(syscall(NR_AOK_EVENT_ACK, fd, 2), EINVAL,
		   "ack unissued event id");

	/* Replay: a duplicated handle has a fresh cursor, unacked events
	 * remain readable until acknowledged. */
	attr.first_ns = 30000000;
	fd2 = make_source(&attr);
	if (fd2 < 0)
		finish(1);
	usleep(90000);
	ok = read_event(fd2, &ev) == sizeof(ev) && ev.event_id == 1;
	replay_id = ev.event_id;
	fd3 = syscall(NR_AOK_DUPLICATE, fd2, AOK_RIGHTS_SOURCE_INITIAL);
	ok = ok && fd3 >= 0 && read_event(fd3, &ev) == sizeof(ev) &&
		ev.event_id == replay_id;
	ksft_test_result(ok, "unacked event replays on a fresh handle\n");
	{
		int shared = dup(fd2);

		ok = shared >= 0 && read_event(shared, &ev) == 0;
		ksft_test_result(ok, "dup shares the consumed cursor\n");
		close(shared);
	}
	ok = !syscall(NR_AOK_EVENT_ACK, fd2, replay_id) &&
	     read_event(fd3, &ev) == 0;
	ksft_test_result(ok, "ack removes the event for every handle\n");
	close(fd3);
	close(fd2);
	close(fd);

	/* Bind stamps the application identity onto later events. */
	attr.first_ns = 10000000;
	fd = make_source(&attr);
	if (fd < 0)
		finish(1);
	ok = !syscall(NR_AOK_EVENT_BIND, fd, 42, AOK_WAKE_ON_EVENT);
	usleep(60000);
	ok = ok && read_event(fd, &ev) == sizeof(ev) &&
		ev.application_id == 42;
	ksft_test_result(ok, "bind stamps application id on events\n");
	EXPECT_ERR(syscall(NR_AOK_EVENT_BIND, fd, 7, AOK_WAKE_MANUAL), EBUSY,
		   "pending event identity cannot be rebound");
	ok = !syscall(NR_AOK_EVENT_ACK, fd, ev.event_id) &&
	      !syscall(NR_AOK_EVENT_BIND, fd, 7, AOK_WAKE_MANUAL);
	ksft_test_result(ok, "rebinding replaces the application id\n");
	EXPECT_ERR(syscall(NR_AOK_EVENT_BIND, fd, 1, 3), EINVAL,
		   "unknown wake policy");
	ro_fd = syscall(NR_AOK_DUPLICATE, fd, AOK_RIGHT_READ);
	EXPECT_ERR(syscall(NR_AOK_EVENT_BIND, ro_fd, 1, 1), EACCES,
		   "bind right required");
	EXPECT_ERR(syscall(NR_AOK_EVENT_ACK, ro_fd, 1), EACCES,
		   "ack right required");
	wo_fd = syscall(NR_AOK_DUPLICATE, fd, AOK_RIGHT_WRITE);
	EXPECT_ERR(read(wo_fd, &ev, sizeof(ev)), EACCES, "read right required");
	close(ro_fd);
	close(wo_fd);
	close(fd);

	/* Structural validation of the creation record. */
	attr.first_ns = 0;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, NULL, &info), EFAULT,
		   "null source attr");
	attr.size = sizeof(attr) - 1;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EINVAL,
		   "short source attr");
	attr.size = page + 1;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EINVAL,
		   "oversized source attr");
	attr.size = sizeof(attr);
	attr.reserved = 1;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EINVAL,
		   "source reserved field");
	attr.reserved = 0;
	attr.kind = 99;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EINVAL,
		   "unknown source kind");
	attr.kind = AOK_EVENT_SOURCE_TIMER;
	attr.flags = 5;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EINVAL,
		   "unknown timer flags");
	attr.flags = AOK_TIMER_ONESHOT;
	attr.interval_ns = 1;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EINVAL,
		   "oneshot with interval");
	attr.flags = AOK_TIMER_PERIODIC;
	attr.interval_ns = 0;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EINVAL,
		   "periodic without interval");
	attr.interval_ns = 1000000;
	attr.parent_job_fd = -1;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EBADF,
		   "invalid parent capability");
	raw_fd = open("/dev/null", O_RDWR);
	attr.parent_job_fd = raw_fd;
	EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EINVAL,
		   "non-capability parent");
	close(raw_fd);
	{
		struct aok_aproc_create_result ares = {};
		struct aok_object_info ainfo = {};
		int aproc_fd, weak;

		create_args.parent_job_fd = root_fd;
		ainfo.size = sizeof(ainfo);
		ares.size = sizeof(ares);
		aproc_fd = syscall(NR_AOK_CREATE, &create_args, &ainfo, &ares);
		if (aproc_fd == 0)
			for (;;)
				pause();
		weak = syscall(NR_AOK_DUPLICATE, aproc_fd,
			       AOK_RIGHT_INSPECT | AOK_RIGHT_DUPLICATE);
		attr.parent_job_fd = weak;
		EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &attr, &info), EACCES,
			   "manage child right required");
		close(weak);
		close(aproc_fd);
	}

	/* Backpressure: a full queue coalesces expiries, ack flushes them. */
	attr.parent_job_fd = root_fd;
	attr.first_ns = 1000000;
	fd = make_source(&attr);
	if (fd < 0)
		finish(1);
	usleep(1000000);
	seen = 0;
	seq_prev = 0;
	flush_coalesced = 0;
	while (read_event(fd, &ev) == sizeof(ev)) {
		if (ev.event_seq <= seq_prev || ev.coalesced != 1)
			flush_coalesced = -1;
		seq_prev = ev.event_seq;
		if (!seen)
			first_id = ev.event_id;
		seen++;
	}
	ok = seen >= 60 && seen <= AOK_EVENT_SOURCE_DEPTH && flush_coalesced == 0;
	ksft_test_result(ok, "queue fills with ordered events (%d seen)\n", seen);
	ok = !syscall(NR_AOK_EVENT_ACK, fd, first_id) &&
	     read_event(fd, &ev) == sizeof(ev) && ev.coalesced >= 20;
	ksft_test_result(ok, "ack flushes coalesced expiries (%u merged)\n",
			 ev.coalesced);
	ok = !syscall(NR_AOK_EVENT_ACK, fd, ev.event_id);
	ksft_test_result(ok, "coalesced event acks normally\n");

	/* Handles keep the source alive; the last close stops the timer. */
	fd2 = syscall(NR_AOK_DUPLICATE, fd, AOK_RIGHTS_SOURCE_INITIAL);
	ok = fd2 >= 0;
	close(fd);
	usleep(50000);
	ok = ok && read_event(fd2, &ev) >= 0;
	ksft_test_result(ok, "source survives one handle close\n");
	close(fd2);

	/* Closing an armed periodic source must not disturb later objects. */
	attr.first_ns = 5000000;
	fd = make_source(&attr);
	close(fd);
	fd = make_source(&attr);
	ksft_test_result(fd >= 0, "sources allocate after an armed close\n");
	if (fd >= 0)
		close(fd);
	drain_zombies();
	close(root_fd);
	ksft_print_cnts();
	finish(ksft_get_fail_cnt() != 0 ||
	       ksft_test_num() != AOK_EVENTSRC_TEST_PLAN);
	return 0;
}
