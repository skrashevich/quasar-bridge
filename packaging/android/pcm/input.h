// SPDX-License-Identifier: Apache-2.0
#ifndef QUASAR_ANDROID_PCM_INPUT_H
#define QUASAR_ANDROID_PCM_INPUT_H
#include <errno.h>
#include <poll.h>
#include <signal.h>
#include <stddef.h>
#include <unistd.h>

static ssize_t pcm_read_block(int fd, void *buffer, size_t capacity,
                              const volatile sig_atomic_t *stopped) {
    size_t used = 0;
    while (used < capacity) {
        if (*stopped) { errno = ECANCELED; return -1; }
        struct pollfd input = {fd, POLLIN, 0};
        int ready = poll(&input, 1, 50);
        if (*stopped) { errno = ECANCELED; return -1; }
        if (ready < 0) { if (errno == EINTR) continue; return -1; }
        if (ready == 0) continue;
        if (input.revents & (POLLERR | POLLNVAL)) { errno = EIO; return -1; }
        ssize_t count = read(fd, (unsigned char *)buffer + used, capacity - used);
        if (count < 0) { if (errno == EINTR) continue; return -1; }
        if (count == 0) break;
        used += (size_t)count;
    }
    if (used % 4 != 0) { errno = EINVAL; return -1; }
    return (ssize_t)used;
}
#endif
