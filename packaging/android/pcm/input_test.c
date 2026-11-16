// SPDX-License-Identifier: Apache-2.0
#define _POSIX_C_SOURCE 200809L
#include "input.h"
#include <assert.h>
#include <string.h>
#include <sys/wait.h>
#include <time.h>

static volatile sig_atomic_t stopped;
static void stop_reading(int signal_number) { (void)signal_number; stopped = 1; }

int main(void) {
    alarm(5);
    int fds[2];
    assert(pipe(fds) == 0);
    pid_t writer = fork();
    assert(writer >= 0);
    if (writer == 0) {
        close(fds[0]);
        unsigned char expected[64];
        for (unsigned i = 0; i < sizeof(expected); ++i) expected[i] = (unsigned char)i;
        for (size_t offset = 0; offset < sizeof(expected);) {
            size_t count = offset % 7 + 1;
            if (count > sizeof(expected) - offset) count = sizeof(expected) - offset;
            assert(write(fds[1], expected + offset, count) == (ssize_t)count);
            offset += count;
            struct timespec delay = {0, 1000000};
            nanosleep(&delay, NULL);
        }
        close(fds[1]);
        _exit(0);
    }
    close(fds[1]);
    unsigned char block[32];
    for (unsigned part = 0; part < 2; ++part) {
        assert(pcm_read_block(fds[0], block, sizeof(block), &stopped) == 32);
        for (unsigned i = 0; i < sizeof(block); ++i) assert(block[i] == part * 32 + i);
    }
    assert(pcm_read_block(fds[0], block, sizeof(block), &stopped) == 0);
    close(fds[0]);
    int status;
    assert(waitpid(writer, &status, 0) == writer && WIFEXITED(status) && WEXITSTATUS(status) == 0);

    assert(pipe(fds) == 0);
    assert(write(fds[1], "12345678", 8) == 8);
    close(fds[1]);
    assert(pcm_read_block(fds[0], block, sizeof(block), &stopped) == 8);
    assert(memcmp(block, "12345678", 8) == 0);
    assert(pcm_read_block(fds[0], block, sizeof(block), &stopped) == 0);
    close(fds[0]);

    assert(pipe(fds) == 0);
    assert(write(fds[1], "12345", 5) == 5);
    close(fds[1]);
    errno = 0;
    assert(pcm_read_block(fds[0], block, sizeof(block), &stopped) == -1 && errno == EINVAL);
    close(fds[0]);

    struct sigaction action = {0};
    action.sa_handler = stop_reading;
    sigemptyset(&action.sa_mask);
    assert(sigaction(SIGTERM, &action, NULL) == 0);
    assert(pipe(fds) == 0);
    writer = fork();
    assert(writer >= 0);
    if (writer == 0) {
        close(fds[0]);
        struct timespec delay = {0, 20000000};
        nanosleep(&delay, NULL);
        kill(getppid(), SIGTERM);
        nanosleep(&delay, NULL);
        _exit(0);
    }
    close(fds[1]);
    struct timespec before, after;
    clock_gettime(CLOCK_MONOTONIC, &before);
    assert(pcm_read_block(fds[0], block, sizeof(block), &stopped) == -1 && errno == ECANCELED);
    clock_gettime(CLOCK_MONOTONIC, &after);
    assert((after.tv_sec - before.tv_sec) * 1000000000LL + after.tv_nsec - before.tv_nsec < 500000000LL);
    close(fds[0]);
    assert(waitpid(writer, &status, 0) == writer && WIFEXITED(status) && WEXITSTATUS(status) == 0);
    return 0;
}
