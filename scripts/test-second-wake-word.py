#!/usr/bin/env python3
"""Проверка scripts/second-wake-word.sh на временных каталогах, без колонки."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parent / "second-wake-word.sh"
CFG = '{"common":{"a":1},"aliced":{"vadEnabled":false,"jingle":false},"zeta":{"aliced_like":1}}'


class SecondWakeWordTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.cfg = root / "quasar.cfg"
        self.cfg.write_text(CFG, encoding="utf-8")
        self.models = root / "spotter_models"
        for word in ("alisa", "yandex"):
            d = self.models / "activation" / word
            (d / "tts-timings-info").mkdir(parents=True)
            (d / "acoustic_model.nnet").write_bytes(word.encode() * 10)
            (d / "words.txt").write_text(word, encoding="utf-8")
        self.user = root / "user_config.dat"
        self.user.write_text('xx{"account_config":{"spotter":"alisa","jingle":false}}', encoding="utf-8")
        self.env = dict(os.environ, QUASAR_NO_REMOUNT="1", QUASAR_CFG=str(self.cfg),
                        QUASAR_SPOTTER_MODELS=str(self.models), QUASAR_USER_CONFIG=str(self.user),
                        QUASAR_DATA_DIR=str(root / "state"))
        self.slot = self.models / "custom_activation"

    def tearDown(self):
        self.tmp.cleanup()

    def run_script(self, *args):
        return subprocess.run(["sh", str(SCRIPT), *args], env=self.env, capture_output=True, text=True, timeout=20)

    def test_enable_uses_the_word_not_selected_in_the_app_and_disable_restores_the_file(self):
        result = self.run_script("enable")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        config = json.loads(self.cfg.read_text(encoding="utf-8"))
        self.assertEqual(config["aliced"]["customActivation"], {"forceEnable": True, "forcePhrase": "yandex"})
        self.assertEqual(config["aliced"]["vadEnabled"], False)
        self.assertEqual(config["zeta"], {"aliced_like": 1})
        self.assertEqual((self.slot / "acoustic_model.nnet").read_bytes(), b"yandex" * 10)
        self.assertTrue((self.slot / ".second-wake-word").exists())
        self.assertTrue((self.slot / "tts-timings-info").is_dir())
        self.assertIn("customActivation", self.run_script("status").stdout)
        result = self.run_script("disable")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(json.loads(self.cfg.read_text(encoding="utf-8")), json.loads(CFG))
        self.assertFalse(self.slot.exists())

    def test_other_selected_word_gives_the_other_model(self):
        self.user.write_text('{"spotter":"yandex"}', encoding="utf-8")
        self.assertEqual(self.run_script("enable").returncode, 0)
        self.assertEqual((self.slot / "words.txt").read_text(encoding="utf-8"), "alisa")

    def test_enable_twice_is_refused_without_changes(self):
        self.assertEqual(self.run_script("enable", "yandex").returncode, 0)
        before = self.cfg.read_text(encoding="utf-8")
        result = self.run_script("enable", "alisa")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.cfg.read_text(encoding="utf-8"), before)
        self.assertEqual((self.slot / "words.txt").read_text(encoding="utf-8"), "yandex")

    def test_foreign_slot_directory_is_never_touched(self):
        self.slot.mkdir()
        (self.slot / "keep.txt").write_text("мой", encoding="utf-8")
        result = self.run_script("enable", "yandex")
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.cfg.read_text(encoding="utf-8"), CFG)
        self.assertEqual(self.run_script("disable").returncode, 0)
        self.assertEqual((self.slot / "keep.txt").read_text(encoding="utf-8"), "мой")

    def test_unexpected_config_layout_and_unknown_word_change_nothing(self):
        self.cfg.write_text('{"aliced":{"a":1},"x":{"aliced":{"b":2}}}', encoding="utf-8")
        before = self.cfg.read_text(encoding="utf-8")
        self.assertNotEqual(self.run_script("enable", "yandex").returncode, 0)
        self.assertEqual(self.cfg.read_text(encoding="utf-8"), before)
        self.cfg.write_text(CFG, encoding="utf-8")
        self.assertNotEqual(self.run_script("enable", "jarvis").returncode, 0)
        self.user.write_text("{}", encoding="utf-8")
        self.assertNotEqual(self.run_script("enable").returncode, 0)
        self.assertEqual(self.cfg.read_text(encoding="utf-8"), CFG)
        self.assertFalse(self.slot.exists())

    def test_original_config_is_saved_once(self):
        self.assertEqual(self.run_script("enable", "yandex").returncode, 0)
        saved = Path(self.env["QUASAR_DATA_DIR"]) / "second-wake-word" / "quasar.cfg.orig"
        self.assertEqual(saved.read_text(encoding="utf-8"), CFG)
        self.assertEqual(self.run_script("disable").returncode, 0)
        self.assertEqual(self.run_script("enable", "yandex").returncode, 0)
        self.assertEqual(saved.read_text(encoding="utf-8"), CFG)

    def test_script_has_valid_posix_sh_syntax(self):
        self.assertEqual(subprocess.run(["sh", "-n", str(SCRIPT)]).returncode, 0)


if __name__ == "__main__":
    unittest.main()
