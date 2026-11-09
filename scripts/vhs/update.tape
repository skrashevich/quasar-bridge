Source scripts/vhs/common.tape
Output docs/images/update.gif

Hide
Type "cd runtime/vhs-kuyase; unset PROMPT_COMMAND; PS1='$ '; clear; echo 'QUASAR BRIDGE · ОБНОВЛЕНИЕ В КУЯСЭ'; echo 'Станция 2 · сохранение существующих настроек'; echo"
Enter
Sleep 500ms
Show
Sleep 2s
Type "sh install.sh root@127.0.0.1 -p 2222 -F ssh_config"
Enter
Wait /Изменить подключение к нейросети/
Sleep 3s
Enter
Wait /^\$$/
Sleep 2s
Type "ssh -F ssh_config root@127.0.0.1 'sh /data/opt/quasar-bridge/app/control.sh status'"
Enter
Wait /^\$$/
Sleep 6s
