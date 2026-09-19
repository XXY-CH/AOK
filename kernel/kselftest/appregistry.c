// SPDX-License-Identifier: GPL-2.0-only
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/aok.h>
#include <poll.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
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

#define NR_AOK_ROOT_CLAIM __NR_aok_root_cap_claim
#define NR_AOK_SOURCE_CREATE __NR_aok_event_source_create
#define NR_AOK_EVENT_ACK __NR_aok_event_ack
#define NR_AOK_APP_CREATE __NR_aok_application_create
#define NR_AOK_APP_SNAPSHOT __NR_aok_application_snapshot
#define NR_AOK_APP_RESTORE __NR_aok_application_restore
#define NR_AOK_DUPLICATE __NR_aok_handle_duplicate

_Static_assert(sizeof(struct aok_application_attr) == 40, "application attr");
_Static_assert(sizeof(struct aok_event_app) == 16, "event attach");
_Static_assert(sizeof(struct aok_event_record) == 72, "event record");

#define AOK_APPREG_TEST_PLAN 45

static int root_fd = -1;
static struct aok_object_info info = {};

#define EXPECT_ERR(expr, error, name) do { \
	errno = 0; \
	long result = (expr); \
	int saved_errno = errno; \
	ksft_test_result(result == -1 && saved_errno == (error), \
			 "%s (ret=%ld errno=%d)\n", name, result, saved_errno); \
	if (result >= 0 && result != 0) close(result); \
} while (0)

static void finish(int fail)
{
	printf("AOK_APPREG_TEST=%s\n", fail ? "fail" : "pass");
	fflush(stdout);
	if (getpid() == 1) {
		reboot(RB_POWER_OFF);
		for (;;)
			pause();
	}
	exit(fail);
}

static int make_source(unsigned int kind, uint64_t first_ns)
{
	struct aok_event_source_attr attr = {
		.size = sizeof(attr), .parent_job_fd = root_fd,
		.kind = kind, .first_ns = first_ns,
	};

	info.size = sizeof(info);
	return syscall(NR_AOK_SOURCE_CREATE, &attr, &info);
}

static int open_app(uint64_t application_id, int parent_fd)
{
	struct aok_application_attr attr = {
		.size = sizeof(attr), .parent_job_fd = parent_fd,
		.application_id = application_id,
	};

	info.size = sizeof(info);
	return syscall(NR_AOK_APP_CREATE, &attr, &info);
}

static int attach_app(int source_fd, int app_fd, uint64_t application_id)
{
	struct aok_event_app attach = {
		.application_id = application_id, .app_fd = app_fd,
	};

	return ioctl(source_fd, AOK_EVENT_ATTACH_APP, &attach);
}

static int read_event(int fd, struct aok_event_record *rec)
{
	memset(rec, 0, sizeof(*rec));
	return read(fd, rec, sizeof(*rec));
}

static int app_ack(int app_fd, uint64_t event_id)
{
	return ioctl(app_fd, AOK_APP_ACK, event_id);
}

static long snapshot(int app_fd, struct aok_event_record *recs, uint64_t count)
{
	return syscall(NR_AOK_APP_SNAPSHOT, app_fd, recs, count);
}

static int revents_of(int fd)
{
	struct pollfd p = { .fd = fd, .events = POLLIN };

	return poll(&p, 1, 0) < 0 ? -1 : p.revents;
}

int main(int argc, char **argv)
{
	struct aok_event_record ev, second, third, recs[3], restore[2];
	static struct aok_event_record big[AOK_APP_QUEUE_DEPTH];
	struct aok_event_target target = {
		.application_id = 999, .aproc_fd = -1,
		.wake_policy = AOK_WAKE_MANUAL,
	};
	struct aok_event_source_attr probe = {};
	pid_t pid;
	int status, app_fd, app_fd2, dup_fd, lsfs, lsfs2, port;
	int app901, src901, app902, src902, app903, timer, p2, wo, narrow;
	unsigned int i, drained;
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
		ksft_set_plan(5);
		EXPECT_ERR(syscall(NR_AOK_ROOT_CLAIM), ENOSYS,
			   "disabled claim");
		EXPECT_ERR(syscall(NR_AOK_APP_CREATE, &probe, &info), ENOSYS,
			   "disabled application create");
		EXPECT_ERR(syscall(NR_AOK_APP_SNAPSHOT, -1, NULL, 0), ENOSYS,
			   "disabled application snapshot");
		EXPECT_ERR(syscall(NR_AOK_APP_RESTORE, -1, NULL, 0), ENOSYS,
			   "disabled application restore");
		EXPECT_ERR(syscall(NR_AOK_SOURCE_CREATE, &probe, &info), ENOSYS,
			   "disabled source create");
		ksft_print_cnts();
		finish(ksft_get_fail_cnt() != 0);
	}
	if (getpid() != 1)
		ksft_exit_skip("run as initial PID 1 in the QEMU test initramfs\n");
	ksft_set_plan(AOK_APPREG_TEST_PLAN);

	root_fd = syscall(NR_AOK_ROOT_CLAIM);
	ksft_test_result(root_fd >= 0, "PID1 claims root capability\n");

	app_fd = open_app(900, root_fd);
	ksft_test_result(app_fd >= 0 && info.type == AOK_OBJECT_APPLICATION &&
			 info.aid > 0, "application fd created\n");
	if (app_fd < 0)
		finish(1);
	EXPECT_ERR(open_app(0, root_fd), EINVAL,
		   "application id must be non-zero");
	EXPECT_ERR(open_app(902, app_fd), EINVAL,
		   "application parent must be root or aproc");

	lsfs = make_source(AOK_EVENT_SOURCE_LSFS, 0);
	ksft_test_result(lsfs >= 0 && info.type == AOK_OBJECT_EVENT_SOURCE,
			 "LSFS source created\n");
	if (lsfs < 0)
		finish(1);
	ksft_test_result(!attach_app(lsfs, app_fd, 900),
			 "source attaches to application\n");
	EXPECT_ERR(attach_app(lsfs, app_fd, 901), EINVAL,
		   "attach id must match the fd");
	ksft_test_result(!ioctl(lsfs, AOK_EVENT_POST, 0xc0ffeeULL),
			 "LSFS post carries commit cursor\n");
	ok = read_event(lsfs, &ev) == sizeof(ev) &&
	     ev.application_id == 900 && ev.kind == AOK_EVENT_KIND_LSFS &&
	     ev.data == 0xc0ffee && ev.event_id == 1 && ev.event_seq == 1;
	ksft_test_result(ok, "attached source reads durable event\n");

	app_fd2 = open_app(900, root_fd);
	ok = app_fd2 >= 0 && read_event(app_fd2, &second) == sizeof(second) &&
	     second.event_id == 1 && second.kind == AOK_EVENT_KIND_LSFS;
	ksft_test_result(ok, "idempotent create shares durable queue\n");
	dup_fd = syscall(NR_AOK_DUPLICATE, app_fd2, AOK_RIGHT_READ);
	ok = dup_fd >= 0 && read_event(dup_fd, &third) == sizeof(third) &&
	     third.event_id == 1;
	ksft_test_result(ok, "application duplicate replays\n");
	ok = !app_ack(app_fd2, 1) && read_event(dup_fd, &ev) == 0;
	ksft_test_result(ok, "app ack retires durable event\n");
	close(dup_fd);
	close(app_fd2);

	port = make_source(AOK_EVENT_SOURCE_PORT, 0);
	ok = port >= 0 && !attach_app(port, app_fd, 900) &&
	     !ioctl(port, AOK_EVENT_POST, 0) &&
	     read_event(port, &ev) == sizeof(ev) &&
	     ev.kind == AOK_EVENT_KIND_PORT && ev.event_seq == 2;
	ksft_test_result(ok, "port source shares application sequence\n");

	close(port);
	lsfs2 = make_source(AOK_EVENT_SOURCE_LSFS, 0);
	ok = lsfs2 >= 0 && !attach_app(lsfs2, app_fd, 900) &&
	     !ioctl(lsfs2, AOK_EVENT_POST, 0x99) &&
	     read_event(lsfs2, &ev) == sizeof(ev) && ev.event_seq == 2 &&
	     read_event(lsfs2, &ev) == sizeof(ev) && ev.event_seq == 3 &&
	     ev.kind == AOK_EVENT_KIND_LSFS && ev.data == 0x99;
	ksft_test_result(ok, "durable events survive source release\n");

	pid = fork();
	if (pid == 0) {
		int afd = open_app(900, root_fd);
		int src = make_source(AOK_EVENT_SOURCE_LSFS, 0);
		int okc = afd >= 0 && src >= 0 && !attach_app(src, afd, 900) &&
			  !ioctl(src, AOK_EVENT_POST, 0x42);

		_exit(okc ? 0 : 1);
	}
	waitpid(pid, &status, 0);
	dup_fd = syscall(NR_AOK_DUPLICATE, app_fd, AOK_RIGHT_READ);
	ok = dup_fd >= 0 && WIFEXITED(status) && !WEXITSTATUS(status) &&
	     read_event(dup_fd, &ev) == sizeof(ev) && ev.event_seq == 2 &&
	     read_event(dup_fd, &ev) == sizeof(ev) && ev.event_seq == 3 &&
	     read_event(dup_fd, &ev) == sizeof(ev) && ev.event_seq == 4 &&
	     ev.data == 0x42;
	ksft_test_result(ok, "cross-process post replays to parent\n");
	close(dup_fd);

	ksft_test_result(snapshot(app_fd, NULL, 0) == 3,
			 "snapshot counts pending events\n");
	ok = syscall(NR_AOK_APP_SNAPSHOT, app_fd, recs, 3) == 3 &&
	     recs[0].event_seq == 2 && recs[1].event_seq == 3 &&
	     recs[2].event_seq == 4 && recs[2].data == 0x42 &&
	     recs[2].application_id == 900;
	ksft_test_result(ok, "snapshot copies ordered records\n");

	app901 = open_app(901, root_fd);
	src901 = make_source(AOK_EVENT_SOURCE_LSFS, 0);
	ok = app901 >= 0 && src901 >= 0 && !attach_app(src901, app901, 901) &&
	     !ioctl(src901, AOK_EVENT_POST, 0x1000) &&
	     read_event(src901, &ev) == sizeof(ev) &&
	     ev.application_id == 901 && ev.event_seq == 1;
	ksft_test_result(ok, "second application feeds its own queue\n");
	EXPECT_ERR(syscall(NR_AOK_EVENT_ACK, src901, 4), EINVAL,
		   "ack is isolated per application");
	ksft_test_result(snapshot(app901, NULL, 0) == 1,
			 "isolation leaves owning queue intact\n");

	dup_fd = syscall(NR_AOK_DUPLICATE, app_fd,
			 AOK_RIGHT_INSPECT | AOK_RIGHT_WRITE);
	errno = 0;
	ok = dup_fd >= 0 && read_event(dup_fd, &ev) == -1 &&
	     errno == EACCES;
	ksft_test_result(ok, "application read requires READ\n");
	ksft_test_result(revents_of(dup_fd) == POLLERR,
			 "application poll requires READ\n");
	close(dup_fd);
	wo = syscall(NR_AOK_DUPLICATE, lsfs2, AOK_RIGHT_READ);
	EXPECT_ERR(attach_app(wo, app_fd, 900), EACCES,
		   "attach requires source WRITE");
	close(wo);
	narrow = syscall(NR_AOK_DUPLICATE, app_fd,
			 AOK_RIGHT_INSPECT | AOK_RIGHT_WRITE);
	EXPECT_ERR(attach_app(lsfs2, narrow, 900), EACCES,
		   "attach requires app READ|WRITE");
	close(narrow);
	EXPECT_ERR(ioctl(app_fd, 0xffffffffU, 0), ENOTTY,
		   "unknown application ioctl");
	dup_fd = syscall(NR_AOK_DUPLICATE, app_fd, AOK_RIGHT_READ);
	EXPECT_ERR(app_ack(dup_fd, 1), EACCES, "app ack requires WRITE");
	close(dup_fd);

	app902 = open_app(902, root_fd);
	src902 = make_source(AOK_EVENT_SOURCE_LSFS, 0);
	ok = app902 >= 0 && src902 >= 0 && !attach_app(src902, app902, 902);
	for (i = 0; ok && i < AOK_APP_QUEUE_DEPTH; i++)
		ok &= !ioctl(src902, AOK_EVENT_POST, i);
	ksft_test_result(ok, "durable queue accepts bounded batch\n");
	EXPECT_ERR(ioctl(src902, AOK_EVENT_POST, 999), EAGAIN,
		   "full durable queue refuses post");
	ok = !app_ack(app902, 1) && !ioctl(src902, AOK_EVENT_POST, 999) &&
	     syscall(NR_AOK_APP_SNAPSHOT, app902, big, AOK_APP_QUEUE_DEPTH) == AOK_APP_QUEUE_DEPTH &&
	     big[0].event_id == 2 && big[AOK_APP_QUEUE_DEPTH - 1].event_id == 129;
	ksft_test_result(ok, "refused post consumes no identity\n");
	drained = 0;
	while (read_event(app902, &ev) == sizeof(ev)) {
		ok &= !app_ack(app902, ev.event_id);
		drained++;
	}
	ksft_test_result(ok && drained == AOK_APP_QUEUE_DEPTH &&
			 snapshot(app902, NULL, 0) == 0,
			 "application drains and snapshots empty\n");

	memset(restore, 0, sizeof(restore));
	restore[0].size = sizeof(restore[0]);
	restore[0].application_id = 902;
	restore[0].event_id = 500;
	restore[0].event_seq = 10;
	restore[0].kind = AOK_EVENT_KIND_LSFS;
	restore[0].coalesced = 1;
	restore[0].data = 7;
	restore[1] = restore[0];
	restore[1].event_id = 501;
	restore[1].event_seq = 11;
	restore[1].data = 8;
	ksft_test_result(!syscall(NR_AOK_APP_RESTORE, app902, restore, 2),
			 "restore injects persisted records\n");
	ok = read_event(src902, &ev) == sizeof(ev) && ev.event_seq == 10 &&
	     ev.data == 7 && read_event(src902, &ev) == sizeof(ev) &&
	     ev.event_seq == 11 && ev.data == 8;
	ksft_test_result(ok, "restored events replay through source\n");
	ok = !ioctl(src902, AOK_EVENT_POST, 0x77) &&
	     syscall(NR_AOK_APP_SNAPSHOT, app902, big, AOK_APP_QUEUE_DEPTH) == 3 &&
	     big[2].event_id == 502 && big[2].event_seq == 130 &&
	     big[2].data == 0x77;
	ksft_test_result(ok, "restore continues identity space\n");
	EXPECT_ERR(syscall(NR_AOK_APP_RESTORE, app902, restore, 1), EBUSY,
		   "restore refuses non-empty queue");
	restore[0].application_id = 903;
	EXPECT_ERR(syscall(NR_AOK_APP_RESTORE, app902, restore, 2), EINVAL,
		   "restore rejects foreign application");
	restore[0].application_id = 902;
	restore[1].event_id = 500;
	EXPECT_ERR(syscall(NR_AOK_APP_RESTORE, app902, restore, 2), EINVAL,
		   "restore rejects non-monotonic records");
	EXPECT_ERR(syscall(NR_AOK_APP_RESTORE, app902, NULL,
			   AOK_APP_QUEUE_DEPTH + 1), EINVAL,
		   "restore rejects oversized batch");

	app903 = open_app(903, root_fd);
	timer = make_source(AOK_EVENT_SOURCE_TIMER, 200000000);
	ok = app903 >= 0 && timer >= 0 && !attach_app(timer, app903, 903);
	usleep(400000);
	close(timer);
	ok &= read_event(app903, &ev) == sizeof(ev) &&
	      ev.kind == AOK_EVENT_KIND_TIMER && ev.event_seq == 1;
	ksft_test_result(ok, "timer expiry is durable after source close\n");

	p2 = make_source(AOK_EVENT_SOURCE_PORT, 0);
	ok = p2 >= 0 && !ioctl(p2, AOK_EVENT_POST, 0);
	EXPECT_ERR(attach_app(p2, app901, 901), EBUSY,
		   "volatile backlog blocks attach");
	ok = read_event(p2, &ev) == sizeof(ev) &&
	     !syscall(NR_AOK_EVENT_ACK, p2, ev.event_id) &&
	     !attach_app(p2, app901, 901);
	ksft_test_result(ok, "attach succeeds after volatile drain\n");
	EXPECT_ERR(ioctl(p2, AOK_EVENT_SET_TARGET, &target), EINVAL,
		   "attached source cannot relabel");
	EXPECT_ERR(attach_app(p2, -1, 901), EINVAL,
		   "detach requires zero application id");

	/* A kernel-produced snapshot must restore verbatim: the snapshot
	 * path owns the reserved-field zeroing the restore validates. */
	app_fd2 = open_app(904, root_fd);
	lsfs2 = make_source(AOK_EVENT_SOURCE_LSFS, 0);
	ok = app_fd2 >= 0 && lsfs2 >= 0 && !attach_app(lsfs2, app_fd2, 904) &&
	     !ioctl(lsfs2, AOK_EVENT_POST, 0xa) &&
	     !ioctl(lsfs2, AOK_EVENT_POST, 0xb) &&
	     syscall(NR_AOK_APP_SNAPSHOT, app_fd2, recs, 3) == 2 &&
	     recs[0].data == 0xa && recs[1].data == 0xb;
	ok = ok && !app_ack(app_fd2, recs[0].event_id) &&
	     !app_ack(app_fd2, recs[1].event_id) &&
	     !syscall(NR_AOK_APP_RESTORE, app_fd2, recs, 2) &&
	     read_event(app_fd2, &ev) == sizeof(ev) && ev.data == 0xa &&
	     read_event(app_fd2, &ev) == sizeof(ev) && ev.data == 0xb;
	ksft_test_result(ok, "snapshot round-trips through restore\n");

	/* An open cursor must not skip a rebound application's low
	 * sequences: the cursor re-checks which queue it indexes. */
	{
		int app905 = open_app(905, root_fd);
		int app906 = open_app(906, root_fd);
		int s905 = make_source(AOK_EVENT_SOURCE_LSFS, 0);
		int s906 = make_source(AOK_EVENT_SOURCE_LSFS, 0);
		struct aok_event_app detach = { .app_fd = -1 };

		ok = app905 >= 0 && app906 >= 0 && s905 >= 0 && s906 >= 0 &&
		     !attach_app(s905, app905, 905) &&
		     !ioctl(s905, AOK_EVENT_POST, 1) &&
		     !ioctl(s905, AOK_EVENT_POST, 2);
		dup_fd = syscall(NR_AOK_DUPLICATE, s905, AOK_RIGHT_READ);
		ok = ok && dup_fd >= 0 &&
		     read_event(dup_fd, &ev) == sizeof(ev) && ev.event_seq == 1;
		ok = ok && !ioctl(s905, AOK_EVENT_ATTACH_APP, &detach) &&
		     !attach_app(s906, app906, 906) &&
		     !ioctl(s906, AOK_EVENT_POST, 7);
		ok = ok && !attach_app(s905, app906, 906) &&
		     read_event(dup_fd, &ev) == sizeof(ev) &&
		     ev.application_id == 906 && ev.event_seq == 1 &&
		     ev.data == 7;
		ksft_test_result(ok, "rebound source replays from cursor reset\n");
		close(dup_fd);
		close(s905);
		close(s906);
		close(app905);
		close(app906);
	}

	/* Detach clears the registry label: later volatile records carry no
	 * stale application identity. */
	p2 = make_source(AOK_EVENT_SOURCE_PORT, 0);
	ok = p2 >= 0 && !attach_app(p2, app901, 901);
	ok = ok && !ioctl(p2, AOK_EVENT_ATTACH_APP,
			  &(struct aok_event_app){ .app_fd = -1 }) &&
	     !ioctl(p2, AOK_EVENT_POST, 0) &&
	     read_event(p2, &ev) == sizeof(ev) && !ev.application_id;
	ksft_test_result(ok, "detach clears the application label\n");

	ksft_print_cnts();
	finish(ksft_get_fail_cnt() != 0 || ksft_test_num() != AOK_APPREG_TEST_PLAN);
	return 0;
}
