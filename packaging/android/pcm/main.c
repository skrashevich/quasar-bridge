// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Quasar Bridge contributors.
#define _POSIX_C_SOURCE 200809L
#include "input.h"
#include <SLES/OpenSLES.h>
#include <SLES/OpenSLES_Android.h>
#include <pthread.h>
#include <stdio.h>
#include <string.h>
#include <time.h>

enum { BLOCKS = 3, MAX_BLOCK_BYTES = 48000 / 50 * 4 };
static volatile sig_atomic_t stopped;
struct output {
    SLObjectItf engine, mix, player;
    SLPlayItf play;
    SLAndroidSimpleBufferQueueItf queue;
    pthread_mutex_t lock;
    pthread_cond_t changed;
    unsigned queued, tail;
    unsigned char buffer[BLOCKS][MAX_BLOCK_BYTES];
};

static void stop_playing(int signal_number) { (void)signal_number; stopped = 1; }
static int sl_ok(SLresult result, const char *step) {
    if (result == SL_RESULT_SUCCESS) return 1;
    fprintf(stderr, "android-pcm: %s failed (%u)\n", step, (unsigned)result);
    return 0;
}
#define SL_CHECK(call, step) do { if (!sl_ok((call), (step))) return -1; } while (0)

// OpenSL invokes this after a buffer is consumed. It never reads stdin or waits
// for the producer; all potentially blocking input stays on the main thread.
static void consumed(SLAndroidSimpleBufferQueueItf queue, void *context) {
    (void)queue;
    struct output *output = context;
    pthread_mutex_lock(&output->lock);
    if (output->queued > 0) --output->queued;
    pthread_cond_signal(&output->changed);
    pthread_mutex_unlock(&output->lock);
}

static int open_output(struct output *output, unsigned rate) {
    SLEngineItf engine;
    SL_CHECK(slCreateEngine(&output->engine, 0, NULL, 0, NULL, NULL), "create engine");
    SL_CHECK((*output->engine)->Realize(output->engine, SL_BOOLEAN_FALSE), "realize engine");
    SL_CHECK((*output->engine)->GetInterface(output->engine, SL_IID_ENGINE, &engine), "engine interface");
    SL_CHECK((*engine)->CreateOutputMix(engine, &output->mix, 0, NULL, NULL), "create mixer");
    SL_CHECK((*output->mix)->Realize(output->mix, SL_BOOLEAN_FALSE), "realize mixer");
    SLDataLocator_AndroidSimpleBufferQueue locator = {SL_DATALOCATOR_ANDROIDSIMPLEBUFFERQUEUE, BLOCKS};
    SLDataFormat_PCM format = {SL_DATAFORMAT_PCM, 2, rate * 1000,
        SL_PCMSAMPLEFORMAT_FIXED_16, SL_PCMSAMPLEFORMAT_FIXED_16,
        SL_SPEAKER_FRONT_LEFT | SL_SPEAKER_FRONT_RIGHT, SL_BYTEORDER_LITTLEENDIAN};
    SLDataSource source = {&locator, &format};
    SLDataLocator_OutputMix mixer = {SL_DATALOCATOR_OUTPUTMIX, output->mix};
    SLDataSink sink = {&mixer, NULL};
    const SLInterfaceID interfaces[] = {SL_IID_ANDROIDSIMPLEBUFFERQUEUE};
    const SLboolean required[] = {SL_BOOLEAN_TRUE};
    SL_CHECK((*engine)->CreateAudioPlayer(engine, &output->player, &source, &sink,
        1, interfaces, required), "create player");
    SL_CHECK((*output->player)->Realize(output->player, SL_BOOLEAN_FALSE), "realize player");
    SL_CHECK((*output->player)->GetInterface(output->player, SL_IID_PLAY, &output->play), "play interface");
    SL_CHECK((*output->player)->GetInterface(output->player,
        SL_IID_ANDROIDSIMPLEBUFFERQUEUE, &output->queue), "queue interface");
    SL_CHECK((*output->queue)->RegisterCallback(output->queue, consumed, output), "register callback");
    return 0;
}

static void close_output(struct output *output) {
    if (output->play) sl_ok((*output->play)->SetPlayState(output->play, SL_PLAYSTATE_STOPPED), "stop player");
    if (output->queue) sl_ok((*output->queue)->Clear(output->queue), "clear queue");
    // Destroy joins native callbacks before the callback's context goes away.
    if (output->player) (*output->player)->Destroy(output->player);
    if (output->mix) (*output->mix)->Destroy(output->mix);
    if (output->engine) (*output->engine)->Destroy(output->engine);
}

static struct timespec after_ms(long milliseconds) {
    struct timespec deadline;
    clock_gettime(CLOCK_MONOTONIC, &deadline);
    deadline.tv_nsec += milliseconds * 1000000;
    deadline.tv_sec += deadline.tv_nsec / 1000000000;
    deadline.tv_nsec %= 1000000000;
    return deadline;
}

// Both a stalled full queue and EOF drain are bounded. Short timed waits make
// SIGTERM observable even when another native thread receives the signal.
static int wait_below(struct output *output, unsigned limit) {
    struct timespec deadline = after_ms(2000);
    int result = 0;
    pthread_mutex_lock(&output->lock);
    while (output->queued >= limit && !stopped) {
        struct timespec next = after_ms(50);
        if (next.tv_sec > deadline.tv_sec ||
            (next.tv_sec == deadline.tv_sec && next.tv_nsec > deadline.tv_nsec)) {
            result = -1;
            break;
        }
        int error = pthread_cond_timedwait(&output->changed, &output->lock, &next);
        if (error != 0 && error != ETIMEDOUT) { result = -1; break; }
    }
    pthread_mutex_unlock(&output->lock);
    return result;
}

static int enqueue(struct output *output, const void *data, size_t length) {
    pthread_mutex_lock(&output->lock);
    ++output->queued;
    SLresult result = (*output->queue)->Enqueue(output->queue, data, (SLuint32)length);
    if (result != SL_RESULT_SUCCESS) --output->queued;
    else output->tail = (output->tail + 1) % BLOCKS;
    pthread_mutex_unlock(&output->lock);
    return sl_ok(result, "enqueue PCM") ? 0 : -1;
}

static unsigned options(int argc, char **argv) {
    unsigned rate = 0;
    for (int i = 1; i < argc; ++i) {
        const char *option = argv[i];
        if (strcmp(option, "-q") == 0) continue;
        if (++i == argc) return 0;
        const char *value = argv[i];
        if (strcmp(option, "-r") == 0) {
            if (strcmp(value, "44100") == 0) rate = 44100;
            else if (strcmp(value, "48000") == 0) rate = 48000;
            else return 0;
        } else if (strcmp(option, "-D") == 0) { if (strcmp(value, "default") != 0) return 0;
        } else if (strcmp(option, "-t") == 0) { if (strcmp(value, "raw") != 0) return 0;
        } else if (strcmp(option, "-f") == 0) { if (strcmp(value, "S16_LE") != 0) return 0;
        } else if (strcmp(option, "-c") == 0) { if (strcmp(value, "2") != 0) return 0;
        } else return 0;
    }
    return rate;
}

int main(int argc, char **argv) {
    unsigned rate = options(argc, argv);
    if (!rate) {
        fputs("usage: android-pcm -q -D default -t raw -f S16_LE -c 2 -r 44100|48000\n", stderr);
        return 2;
    }
    struct sigaction action = {0};
    action.sa_handler = stop_playing;
    sigemptyset(&action.sa_mask);
    if (sigaction(SIGTERM, &action, NULL) || sigaction(SIGINT, &action, NULL)) return 1;
    struct output output = {.lock = PTHREAD_MUTEX_INITIALIZER};
    pthread_condattr_t attributes;
    if (pthread_condattr_init(&attributes)) return 1;
    if (pthread_condattr_setclock(&attributes, CLOCK_MONOTONIC) ||
        pthread_cond_init(&output.changed, &attributes)) {
        pthread_condattr_destroy(&attributes);
        return 1;
    }
    pthread_condattr_destroy(&attributes);
    int result = 1, playing = 0;
    if (open_output(&output, rate) != 0) goto cleanup;
    while (!stopped) {
        if (wait_below(&output, BLOCKS) != 0) {
            fputs("android-pcm: playback queue stalled\n", stderr);
            goto cleanup;
        }
        if (stopped) break;
        unsigned char *buffer = output.buffer[output.tail];
        ssize_t length = pcm_read_block(STDIN_FILENO, buffer, rate / 50 * 4, &stopped);
        if (length < 0) {
            if (stopped) break;
            fputs("android-pcm: invalid PCM input\n", stderr);
            goto cleanup;
        }
        if (length == 0) break;
        if (enqueue(&output, buffer, (size_t)length) != 0) goto cleanup;
        pthread_mutex_lock(&output.lock);
        unsigned queued = output.queued;
        pthread_mutex_unlock(&output.lock);
        if (!playing && queued >= 2) {
            if (!sl_ok((*output.play)->SetPlayState(output.play, SL_PLAYSTATE_PLAYING), "start player")) goto cleanup;
            playing = 1;
        }
    }
    if (!stopped) {
        if (!playing && !sl_ok((*output.play)->SetPlayState(output.play, SL_PLAYSTATE_PLAYING), "start drain")) goto cleanup;
        if (wait_below(&output, 1) != 0) {
            fputs("android-pcm: playback drain timed out\n", stderr);
            goto cleanup;
        }
    }
    result = 0;
cleanup:
    close_output(&output);
    pthread_cond_destroy(&output.changed);
    pthread_mutex_destroy(&output.lock);
    return result;
}
