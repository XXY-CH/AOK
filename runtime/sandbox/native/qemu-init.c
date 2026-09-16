#define _GNU_SOURCE
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mount.h>
#include <sys/reboot.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>

int main(void)
{
	setbuf(stdout, NULL);
	mkdir("/proc", 0755);
	mount("proc", "/proc", "proc", 0, NULL);
	mkdir("/input", 0755); mkdir("/output", 0755); mkdir("/secret", 0755);
	close(open("/input/data", O_CREAT | O_WRONLY, 0600));
	close(open("/secret/data", O_CREAT | O_WRONLY, 0600));
	symlink("/secret/data", "/input/escape");
	int fd = open("/secret/data", O_RDONLY);
	dup2(fd, 9); close(fd);
	int failed = 0;
	for (int test = 0; test < 3; test++) {
		pid_t pid = fork();
		if (!pid) {
			if (test == 0) execl("/aok-sandbox", "aok-sandbox", "--read", "/input", "--write", "/output", "--", "/probe", "/input/data", "/output/data", "/secret/data", "/input/escape", NULL);
			if (test == 1) execl("/aok-sandbox", "aok-sandbox", "--net", "--keep-fd", "9", "--read", "/input", "--write", "/output", "--", "/probe", "/input/data", "/output/data", "/secret/data", "/input/escape", "network", NULL);
			execl("/aok-sandbox", "aok-sandbox", "--read", "/missing", "--", "/probe", NULL);
			_exit(126);
		}
		int status = 0;
		if (pid < 0 || waitpid(pid, &status, 0) != pid || !WIFEXITED(status) || WEXITSTATUS(status) != (test == 2 ? 125 : 0)) failed = 1;
	}
	if (failed) puts("AOK_SANDBOX_TEST=fail");
	else puts("AOK_SANDBOX_TEST=pass");
	sync(); reboot(RB_POWER_OFF);
	return 1;
}
