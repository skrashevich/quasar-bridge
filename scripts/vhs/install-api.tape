Source scripts/vhs/common.tape
Output docs/images/install-api.gif

Hide
Type "cd runtime/vhs-kuyase; unset PROMPT_COMMAND; PS1='$ '; clear; echo 'QUASAR BRIDGE · УСТАНОВКА'; echo 'Станция 2 · настоящая прошивка, SSH и нейросеть'; echo"
Enter
Sleep 500ms
Show
Sleep 2s
Type "curl -fsSL -o install.sh https://github.com/skrashevich/quasar-bridge/releases/latest/download/install.sh"
Enter
Wait /^\$$/
Sleep 1s
Type "sh install.sh root@127.0.0.1 -p 2222 -F ssh_config"
Enter
Wait /Адрес API нейросети/
Sleep 1s
Type "http://10.0.2.2:18080/v1"
Enter
Wait /Ключ API/
Sleep 800ms
Enter
Wait /Тип API/
Sleep 1s
Enter
Wait /Номер модели:/
Sleep 2s
Type "1"
Enter
Wait /Секретный URL Home Assistant/
Sleep 1s
Enter
Wait /Подключить Алису/
Sleep 2s
Type "y"
Enter
Wait /Перезагрузить сейчас/
Sleep 2s
Type "n"
Enter
Wait /^\$$/
Sleep 2s
Type "ssh -F ssh_config root@127.0.0.1 'sh /data/opt/quasar-bridge/app/control.sh status'"
Enter
Wait /^\$$/
Sleep 5s
