# Android TV Go Proxy

Связка для быстрого управления Android TV приставкой из iRidium без медленного `adb shell` на каждое нажатие.

Идея такая:

- Go-сервер хранит embedded `android/iridi-keyserver.jar`, поднимает веб-интерфейс, работает как watchdog и предоставляет UDP-прокси.
- Управление из iRidium может идти через UDP-прокси Go-сервера либо напрямую на jar-сервис приставки по UDP/TCP.
- Если jar на приставке пропал, Go-сервер через ADB заново пушит jar в `/data/local/tmp` и запускает его.

## Что где работает

```text
iRidium  ---> UDP 10000       ---> Go proxy ---> UDP 17891 ---> Android приставка
iRidium  ------------------------------------> UDP/TCP 17891 ---> IridiKeyServer.jar
Go UI    ---> HTTP 10000      ---> настройка, scan, watchdog, install/restart
Go       ---> ADB 5555        ---> push jar, start jar, проверка состояния
```

По умолчанию:

- вебморда Go: `http://127.0.0.1:10000`
- UDP-прокси Go: `10000`
- jar на приставке: `17891`
- ADB на приставке: `5555`
- файл состояния: `devices.json`
- jar на приставке: `/data/local/tmp/iridi-keyserver.jar`
- логи jar на приставке: `/data/local/tmp/iridi-keyserver.log`

## Основной сценарий

1. Включить ADB по сети на приставке.
2. Запустить Go-сервер.
3. Открыть вебморду на порту `10000`.
4. Нажать `Сканировать сеть`.
5. Найти приставку и нажать `Запомнить`.
6. Нажать `Install/Restart`, если jar еще не запущен.
7. В iRidium отправлять команды на IP Go-сервера по UDP, порт `10000`, либо напрямую на IP приставки, порт `17891`.

После этого Go-сервер раз в минуту проверяет запомненные приставки. Если jar не отвечает, сервер ищет приставку по ADB, пушит jar и запускает его заново.

## Запуск на Windows

Пример для текущей разработки:

```powershell
$env:ADB_PATH = "$env:LOCALAPPDATA\Android\Sdk\platform-tools\adb.exe"
$env:ADB_SERIAL = "192.168.88.30:5555"
$env:WEB_PORT = "10000"
.\adb-http-proxy.exe
```

`ADB_SERIAL` необязателен. Если он указан, сервер при старте сразу попробует прочитать приставку и добавить ее в `devices.json`.

## Запуск на Linux / Buildroot

На целевом сервере должен быть рабочий бинарь `adb`.

```sh
export ADB_PATH=/root/platform-tools-34.0.4-arm/platform-tools/adb
export WEB_PORT=10000
./adb-http-proxy
```

Если известен адрес приставки:

```sh
export ADB_SERIAL=192.168.88.30:5555
./adb-http-proxy
```

По умолчанию `ADB_PATH` уже смотрит на:

```text
/root/platform-tools-34.0.4-arm/platform-tools/adb
```

## Сборка Go-сервера

Windows:

```powershell
go build -o adb-http-proxy.exe Main.go
```

Linux ARM64:

```powershell
$env:GOOS = "linux"
$env:GOARCH = "arm64"
go build -o adb-http-proxy Main.go
```

Linux ARMv7:

```powershell
$env:GOOS = "linux"
$env:GOARCH = "arm"
$env:GOARM = "7"
go build -o adb-http-proxy Main.go
```

Важно: jar встроен в Go-бинарь через `go:embed`. Если менялся `android/iridi-keyserver.jar`, после этого нужно пересобрать Go-бинарь.

## Переменные окружения

| Переменная | По умолчанию | Что делает |
| --- | --- | --- |
| `ADB_PATH` | `/root/platform-tools-34.0.4-arm/platform-tools/adb` | путь к adb |
| `ADB_SERIAL` | пусто | приставка для первичного добавления при старте, например `192.168.88.30:5555` |
| `WEB_PORT` | `10000` | TCP-порт веб-интерфейса и UDP-порт прокси |
| `WATCHDOG_INTERVAL_SEC` | `60` | как часто watchdog проверяет jar |
| `SCAN_CIDR` | авто по локальным интерфейсам | сети для scan, например `192.168.88.0/24` |
| `SCAN_TIMEOUT_MS` | `350` | таймаут проверки портов при scan |
| `JAR_INJECT_MODE` | `1` | режим Android `injectInputEvent` |
| `JAR_DUPLICATE_DROP_MS` | `90` | защита от дублей одинакового keyevent |

## Веб-интерфейс

Открывается на:

```text
http://127.0.0.1:10000
```

Кнопки:

- `Сканировать сеть` - ищет устройства с открытым ADB `5555` и/или jar `17891`.
- `Запомнить` - сохраняет приставку в `devices.json`.
- `Install/Restart` - пушит jar через ADB и запускает его.
- `Test` - отправляет команду из поля ввода напрямую в jar по UDP. Всплывающего окна нет, результат пишется в логи вебморды.
- `Forget` - удаляет приставку из `devices.json`.

Старый HTTP-прокси `/adb` удалён. Вместо него Go-сервер принимает команды по UDP на `WEB_PORT` и пересылает их в jar запомненной приставки. Если запомнено несколько приставок, используйте прямое подключение к нужной приставке на `17891`: у UDP-прокси пока нет явного выбора активной цели.

## Формат команд для iRidium

Рекомендуемый режим: UDP на IP приставки, порт `17891`.

Пример для iRidium:

```text
'input keyevent 21',13
```

`13` - это CR. Jar также понимает LF (`10`) и CR+LF. Для UDP окончание строки не критично, но в iRidium лучше оставлять `,13`, чтобы формат был явным.

Примеры:

```text
'input keyevent 21',13
'input keyevent 22',13
'input keyevent ENTER',13
'keyevent BACK',13
'ping',13
'cmd=shell monkey -p ru.kinopoisk.tv 1',13
```

Поддерживаются варианты:

```text
keyevent 21
input keyevent 21
shell input keyevent 21
adb shell input keyevent 21
cmd=shell monkey -p ru.kinopoisk.tv 1
```

Для запуска shell-команд используйте `shell ...`, `adb shell ...` или `cmd=...`.

Например:

```text
'cmd=shell monkey -p ru.kinopoisk.tv 1',13
```

Ответы jar:

```text
OK
OK pong
OK dropped duplicate
ERR ...
```

Если ответ iRidium не нужен, можно отправлять команды с префиксом `noreply`:

```text
'noreply input keyevent 22',13
```

## Что умеет jar

`IridiKeyServer.jar`:

- слушает TCP и UDP на `17891`;
- принимает команды с CR, LF, CR+LF или без окончания строки на UDP;
- инжектит keyevent напрямую через Android `InputManager`;
- умеет несколько keyevent в одной команде, например `input keyevent 21 22`;
- умеет `--longpress`;
- умеет запускать shell-команды с таймаутом 10 секунд;
- пишет логи в stdout/stderr, при запуске Go они уходят в `/data/local/tmp/iridi-keyserver.log`;
- отбрасывает одинаковые keyevent, пришедшие быстрее `JAR_DUPLICATE_DROP_MS`, чтобы не было случайных дублей.

## Root нужен?

Root не нужен для текущей схемы.

Нужно:

- чтобы на приставке был доступен ADB по сети;
- чтобы ADB-сессия могла писать в `/data/local/tmp`;
- чтобы `app_process` мог запустить jar.

Без root jar не ставится как системный Android service и не переживает reboot сам по себе. Эту роль выполняет Go watchdog: когда приставка снова появляется по ADB, он заново запускает jar.

## Если IP приставки поменялся

Go хранит устройство в `devices.json` по Android serial, если serial доступен. При watchdog-проверке сервер:

1. пробует текущий IP и порт jar;
2. если jar не отвечает, ищет приставку через `adb devices`;
3. пробует `adb connect` на известный IP;
4. при ручном scan ищет устройства в локальных `/24` сетях или в `SCAN_CIDR`.

Если приставка перешла на другой IP, нажмите `Сканировать сеть` в вебморде. Лучше закрепить IP в DHCP на роутере.

## Диагностика

Проверить ADB:

```powershell
adb devices -l
adb connect 192.168.88.30:5555
```

Если устройство `offline`, часто помогает:

```powershell
adb kill-server
adb start-server
adb connect 192.168.88.30:5555
```

Проверить jar-порт:

```powershell
Test-NetConnection 192.168.88.30 -Port 17891
```

Посмотреть лог Go:

```powershell
Get-Content .\server.err.log -Tail 100
```

Посмотреть лог jar на приставке:

```powershell
adb -s 192.168.88.30:5555 shell tail -n 100 /data/local/tmp/iridi-keyserver.log
```

Ручной запуск jar через ADB:

```powershell
adb -s 192.168.88.30:5555 push android\iridi-keyserver.jar /data/local/tmp/iridi-keyserver.jar
adb -s 192.168.88.30:5555 shell "setsid sh -c 'CLASSPATH=/data/local/tmp/iridi-keyserver.jar exec app_process / IridiKeyServer 17891 0 1 90 >/data/local/tmp/iridi-keyserver.log 2>&1 < /dev/null' >/dev/null 2>&1 &"
```

## Частые проблемы

### Кнопки в вебморде не работают

Обновите страницу через `Ctrl+F5`. Сервер отдает `Cache-Control: no-store`, но старый браузер мог держать предыдущий HTML.

### В iRidium нет реакции

Проверьте:

- iRidium отправляет на IP приставки, а не на IP Go-сервера;
- протокол UDP или TCP, порт `17891`;
- команда оформлена строкой, например `'input keyevent 22',13`;
- jar отвечает на `ping`;
- firewall/роутер не режет локальный UDP/TCP между контроллером и приставкой.

### Watchdog не может восстановить jar

Проверьте `adb devices -l`. Если приставка `offline`, Go не сможет сделать `adb push`. Нужно восстановить ADB-сессию или перезапустить ADB server.

### После изменения Java ничего не поменялось

Нужно пересобрать `android/iridi-keyserver.jar`, а потом пересобрать Go-бинарь, потому что jar embedded внутри Go.
