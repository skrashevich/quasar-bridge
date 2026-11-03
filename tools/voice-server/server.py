#!/usr/bin/env python3
"""Сервис озвучки для Quasar Bridge: POST /v1/audio/speech -> Ogg/Opus 48 кГц, моно.

Запускается на компьютере в сети колонки. Нужен только Python 3 и ffmpeg.
Голос — это модель Piper (файл .onnx) или любая команда, которая принимает текст
на stdin и пишет аудио на stdout. Примеры — в README рядом.
"""
import argparse
import json
import logging
import shlex
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

MAX_TEXT = 4000
log = logging.getLogger("voice")


def ffmpeg_command() -> list[str]:
    return ["ffmpeg", "-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-ac", "1", "-ar", "48000",
            "-c:a", "libopus", "-b:a", "48k", "-application", "voip", "-frame_duration", "20",
            "-page_duration", "20000", "-f", "ogg", "pipe:1"]


def discover_voices(args) -> dict[str, list[str]]:
    voices = {}
    if args.voices:
        for model in sorted(Path(args.voices).glob("*.onnx")):
            voices[model.stem] = [args.piper, "--model", str(model), "--output_file", "-"]
    for item in args.command or []:
        name, _, command = item.partition("=")
        if not name or not command:
            raise SystemExit("--command нужно указывать как имя=команда")
        voices[name] = shlex.split(command)
    if not voices:
        raise SystemExit("Нет голосов: укажите --voices с моделями .onnx или --command имя=команда")
    return voices


def make_handler(voices, default, token):
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *_):
            pass

        def fail(self, code, message):
            body = json.dumps({"error": {"message": message}}, ensure_ascii=False).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def do_GET(self):
            if self.path.rstrip("/") == "/health":
                return self.fail(200, "ok")
            if self.path.rstrip("/") == "/v1/voices":
                body = json.dumps({"voices": sorted(voices), "default": default}, ensure_ascii=False).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return None
            return self.fail(404, "not found")

        def do_POST(self):
            if self.path.rstrip("/") != "/v1/audio/speech":
                return self.fail(404, "not found")
            size = int(self.headers.get("Content-Length", "0") or 0)
            if size > 1 << 20:
                self.close_connection = True
                return self.fail(413, "request too large")
            raw = self.rfile.read(size)
            if token and self.headers.get("Authorization") != f"Bearer {token}":
                return self.fail(401, "bad token")
            try:
                request = json.loads(raw)
                text = str(request.get("input", "")).strip()
            except (ValueError, AttributeError):
                return self.fail(400, "invalid JSON")
            if not text or len(text) > MAX_TEXT:
                return self.fail(400, "input must be 1-4000 characters")
            name = request.get("voice") or default
            if name not in voices:
                return self.fail(400, f"unknown voice, available: {', '.join(sorted(voices))}")
            return self.speak(text, voices[name], name)

        def speak(self, text, command, name):
            synth = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
            encoder = subprocess.Popen(ffmpeg_command(), stdin=synth.stdout, stdout=subprocess.PIPE)
            synth.stdout.close()

            def feed():
                try:
                    synth.stdin.write(text.encode())
                    synth.stdin.close()
                except BrokenPipeError:
                    pass

            threading.Thread(target=feed, daemon=True).start()
            try:
                # Заголовок уходит с первым звуком: при сбое движка мост ещё может
                # получить ошибку и ответить голосом Алисы.
                first = encoder.stdout.read(4096)
                if not first:
                    log.warning("голос %s не вернул звук", name)
                    return self.fail(500, "synthesis failed")
                self.send_response(200)
                self.send_header("Content-Type", "audio/ogg")
                self.send_header("Transfer-Encoding", "chunked")
                self.end_headers()
                while first:
                    self.wfile.write(b"%x\r\n%s\r\n" % (len(first), first))
                    self.wfile.flush()
                    first = encoder.stdout.read1(16384)
                self.wfile.write(b"0\r\n\r\n")
                log.info("озвучено голосом %s: %d символов", name, len(text))
            except (BrokenPipeError, ConnectionResetError):
                log.info("запрос отменён")
                self.close_connection = True
            finally:
                for process in (synth, encoder):
                    process.kill()
                    process.wait()
            return None

    return Handler


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=8880)
    parser.add_argument("--voices", help="папка с голосами Piper (*.onnx рядом с *.onnx.json)")
    parser.add_argument("--piper", default="piper", help="команда запуска Piper")
    parser.add_argument("--command", action="append", help="свой голос: имя=команда; текст на stdin, аудио на stdout")
    parser.add_argument("--default", help="голос по умолчанию")
    parser.add_argument("--token", help="если задан, запросы должны передавать этот ключ")
    args = parser.parse_args()
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
    voices = discover_voices(args)
    default = args.default or sorted(voices)[0]
    if default not in voices:
        raise SystemExit(f"Голос {default} не найден")
    log.info("голоса: %s; адрес для моста: http://<IP компьютера>:%d/v1", ", ".join(sorted(voices)), args.port)
    ThreadingHTTPServer((args.host, args.port), make_handler(voices, default, args.token)).serve_forever()


if __name__ == "__main__":
    main()
