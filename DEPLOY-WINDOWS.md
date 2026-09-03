# DEPLOY-WINDOWS — mcp-pg-readonly на Windows VPS с нуля

Полный гайд: свежая Windows-машина → работающий read-only SQL-доступ для
ZCode-агента. Рассчитан на воспроизведение без знания истории проекта; все
подводные камни, собранные при эксплуатации на macOS, учтены. Рассчитано на
Windows 10/11 и Windows Server 2019+.

Краткая версия (без объяснений) — в README.md, раздел *Windows (VPS)*.
Хронология решений — AGENTS.md.

## 0. Что получится в итоге

```
ZCode (%USERPROFILE%\.zcode\cli\config.json)
  └─ command → %USERPROFILE%\bin\mcp-pg-readonly-run.cmd     ← wrapper
                ├─ call %USERPROFILE%\.config\mcp-pg-readonly\env.cmd
                │    (set PG_RO_PWD=…  — пароль живёт ТОЛЬКО здесь)
                └─ %USERPROFILE%\bin\mcp-pg-readonly.exe --database …
                      └─ PostgreSQL, роль mcp_ro (SELECT-only)
```

Принципы, которые надо понимать до начала:

- **Граница безопасности — роль `mcp_ro` в самой БД**, а не SQL-парсер в
  сервере. SQL-проверка — это UX-пречек. Поэтому bootstrap роли (раздел 5) —
  не опциональная «лучшие практики», а обязательный шаг, если PG-инстанс новый.
- **Wrapper существует, чтобы пароль не жил в config.json** рядом с ключами
  провайдеров, и чтобы путь `command` оставался стабильным.
- **Один MCP-процесс = одна база** (`--database` обязателен; молчаливого
  дефолта в прод нет). Нужны две базы — два entry в config.json.
- Папка репо называется `mini-mcp`, а модуль/бинарь — `mcp-pg-readonly`
  (заведомо: путь MCP-команды переживает переименования папки).

Агент получит три инструмента: `pg_readonly_query`, `pg_list_tables`,
`pg_describe_table` — с подсказками прямо в описаниях (капы, касты чисел,
psql-обход). Возможности и ограничения — в разделе 9.

## 1. Сеть: как VPS видит PostgreSQL (проверить ПЕРВЫМ)

Прод-инстанс живёт на `192.168.10.7:15432` — это **приватный адрес LAN**.
С постороннего VPS он не маршрутизируется. Варианты, по убыванию простоты:

1. **VPS в той же сети / есть тоннель** (WireGuard, Tailscale, SSH
   port-forward) → оставить `PGHOST=192.168.10.7` (или IP тоннеля),
   `PGSSLMODE=disable` допустим только внутри доверенного тоннеля.
2. **Публичный доступ** → пробросить порт PG, в config.json поставить
   `"PGSSLMODE": "require"` (или `verify-full`), и на сервере PG:
   - добавить IP VPS в `pg_hba.conf`:
     `hostssl  all  mcp_ro  <ip_vps>/32  scram-sha-256`
   - применить: `SELECT pg_reload_conf();` (под админом)
   - открыть порт в firewall PG-хоста.
3. **Свой PG прямо на VPS** → `PGHOST=127.0.0.1`, тогда обязательно раздел 5
   (bootstrap роли с нуля).

Проверка достижимости с VPS (PowerShell — только для диагностики, сам wrapper
всегда запускается под cmd.exe):

```powershell
Test-NetConnection 192.168.10.7 -Port 15432
```

`TcpTestSucceeded : True` — можно продолжать. `False` — чинить сеть до всего
остального, иначе упрётесь в `connection refused/timeout` на selftest.

## 2. Пререквизиты

| Что | Зачем | Установка |
|---|---|---|
| Git for Windows | clone / обновления | `winget install --id Git.Git` или git-scm.com |
| Go **≥ 1.25** | сборка (go.mod: `go 1.25.0`) | `winget install --id GoLang.Go` или MSI с go.dev/dl |
| ZCode | сам агент | как обычно ставите |
| psql | bootstrap роли + psql-bypass (раздел 9) | установщик PostgreSQL от EDB, достаточно «Command Line Tools» |

После установки Git и Go откройте **новое** окно терминала — PATH подхватывается
только в новых сессиях (частый «а почему `go` не найден»). Проверка:

```cmd
git --version
go version
```

## 3. Код и сборка

```cmd
git clone https://github.com/ilkoid/mini-mcp.git %USERPROFILE%\dev\mini-mcp
cd /d %USERPROFILE%\dev\mini-mcp
mkdir %USERPROFILE%\bin 2>nul
go build -o %USERPROFILE%\bin\mcp-pg-readonly.exe .
copy /Y mcp-pg-readonly-run.cmd %USERPROFILE%\bin\mcp-pg-readonly-run.cmd
go test ./...
```

`go test` — юнит-тесты, БД не нужна, должны пройти. Интеграционные
(`PG_TEST_DSN`) опциональны и **никогда** не направляются в прод.

Подводные камни:

- **Не копируйте бинарь с Mac** (`Mach-O arm64`) — он не запустится. `.exe`
  собирается на месте. Исключение — кросс-компиляция:
  `GOOS=windows GOARCH=amd64 go build` с Mac/Linux.
- **CRLF уже управляется `.gitattributes`**: `*.cmd`/`*.bat` в working tree —
  CRLF (канонический формат cmd), всё остальное — LF, `*.exe` — binary.
  Ничего «чинить» не надо. Проверка после клона:
  `git ls-files --eol mcp-pg-readonly-run.cmd` → в worktree `crlf`.
- Установщик Git for Windows ставит `core.autocrlf=true` — это нормально:
  атрибуты из `.gitattributes` приоритетнее глобальной настройки.

## 4. Секреты на Windows

Сгенерируйте пароль — только из hex-символов `[0-9a-f]` (PowerShell):

```powershell
-join ((1..32) | ForEach-Object { '{0:x}' -f (Get-Random -Maximum 16) })
```

Почему именно hex: значение в env.cmd пишется через `set` **без кавычек**, и
любой спецсимвол cmd (пробел, `&`, `^`, `%`, `|`, `<`, `>`) сломает строку или
исказит пароль.

Создайте файл (обратите внимание: расширение `.cmd`, не `.env`):

```cmd
mkdir %USERPROFILE%\.config\mcp-pg-readonly 2>nul
notepad %USERPROFILE%\.config\mcp-pg-readonly\env.cmd
```

Содержимое (`set`-синтаксис cmd, НЕ `export`, НЕ PowerShell `$env:`):

```bat
set PG_RO_PWD=<32-hex пароль>
REM вторая строка нужна ТОЛЬКО если на этом VPS делается bootstrap роли:
set PG_ADMIN_PWD=<пароль postgres>
```

Закройте доступ всем, кроме своего пользователя (аналог chmod 600):

```cmd
icacls "%USERPROFILE%\.config\mcp-pg-readonly\env.cmd" /inheritance:r /grant:r "%USERNAME%:F"
```

Правила и камни:

- **`set` без кавычек.** `set PG_RO_PWD='abc'` запишет значение буквально
  `'abc'` вместе с кавычками. Это камень №1 на Windows.
- **Не `setx`!** Он пишет пароль в реестр (`HKCU\Environment`), откуда его
  видит каждый процесс пользователя.
- Если ZCode работает **от другого пользователя**, чем RDP-сессия — выдать
  ACL этому пользователю, иначе wrapper увидит «empty password».
- `PG_RO_PWD` ≠ `PG_PWD` (админ-пароль загрузчиков poncho-ai) ≠
  `PG_ADMIN_PWD`. Не переиспользовать.
- Если VPS подключается к **существующему** PG (192.168.10.7) — роль `mcp_ro`
  там уже создана; используйте **тот же пароль**, что на Mac (взять из
  `~/.config/mcp-pg-readonly/env`, передать по RDP/менеджеру секретов, не
  через git/чат/issue) — и раздел 5 пропустите.
- Путь к секретам можно переопределить переменной `MCP_PG_SECRETS`, если
  держите несколько файлов (по одному на хост с разными паролями).

## 5. Роль mcp_ro — только если на этом VPS свой PG-инстанс

Под существующий PG роль уже есть — пропускайте. Для своего инстанса,
под админом, по каждой базе (сначала test, потом прод — осознанно):

```cmd
set PG_RO_PWD=<32-hex пароль>
set PG_ADMIN_PWD=<пароль postgres>
"%ProgramFiles%\PostgreSQL\17\bin\psql.exe" -h 127.0.0.1 -p 5432 -U postgres ^
  -d wb_data_test -v ON_ERROR_STOP=1 -v pwd="%PG_RO_PWD%" ^
  -f %USERPROFILE%\dev\mini-mcp\role-bootstrap.sql
```

(путь к psql/порт — под свою установку)

Камни bootstrap:

- Имя именно `mcp_ro`: префикс `pg_` зарезервирован PostgreSQL, `pg_ro`
  создать нельзя.
- Скрипт идемпотентный, пароль передаётся через `-v pwd=` и экранируется
  `%L` — реальный пароль не светится в истории команд.
- **Перед запуском** найдите реального владельца таблиц и подставьте его в
  блок `ALTER DEFAULT PRIVILEGES` (в скрипте по умолчанию `postgres`):
  `SELECT DISTINCT tableowner FROM pg_tables WHERE schemaname='public';`
  Иначе будущие таблицы будут нечитаемы для `mcp_ro`.
- `CONNECTION LIMIT 8` покрывает несколько сессий ZCode + selftest. При
  `too many connections for role "mcp_ro"` — поднимать лимит роли
  (`ALTER ROLE mcp_ro CONNECTION LIMIT 16;`), **не** MaxConns пула
  (иначе голодаете другие сессии).
- `role-bootstrap-layers.sql` — фаза B для несуществующих пока схем, не нужен.

## 6. Ручная проверка до ZCode: selftest

```cmd
%USERPROFILE%\bin\mcp-pg-readonly-run.cmd --selftest --database wb_data_test
```

Ожидание в stderr (и только там):

```
ok: SELECT 1 -> 1 row(s)
ok: numeric probe -> round(numeric) as exact string
ok: write probe rejected as expected (...)
ok: row cap engaged at 500 rows (truncated=true)
selftest: PASS
```

Write probe **обязан** упасть — это сквозная проверка read-only границы.
Затем то же самое с `--database wb_data_prod` (осознанно, отдельным вызовом).

Типичные ошибки здесь: `error: empty password` → кавычки/спецсимвол в env.cmd
или ACL не тому пользователю; `ping: failed to connect` → сеть/pg_hba/firewall
(раздел 1).

## 7. Регистрация в ZCode

Файл — **`%USERPROFILE%\.zcode\cli\config.json`** (именно `cli`; соседний
`~/.zcode/v2/config.json` — конфиг провайдеров, его трогать не надо):

```jsonc
{
  "mcp": {
    "servers": {
      "pg-readonly": {
        "type": "stdio",
        "command": "C:/Users/ВАШ_ЮЗЕР/bin/mcp-pg-readonly-run.cmd",
        "args": ["--database", "wb_data_prod"],
        "timeoutMs": 200000,
        "env": { "PGHOST": "192.168.10.7", "PGPORT": "15432" }
      },
      "pg-readonly-test": {
        "type": "stdio",
        "command": "C:/Users/ВАШ_ЮЗЕР/bin/mcp-pg-readonly-run.cmd",
        "args": ["--database", "wb_data_test"],
        "timeoutMs": 200000,
        "env": { "PGHOST": "192.168.10.7", "PGPORT": "15432" }
      }
    }
  }
}
```

Правила (нарушение любого = сервер молча пропадёт из списка):

- `command` — **строка** с абсолютным путём (не массив — будет
  `command.trim is not a function`), слэши прямые `/`, указывает на
  **launcher `.cmd`**, не на `.exe` напрямую — launcher подхватывает пароль.
- Ключ `env`, не легаси `environment`.
- **`timeoutMs: 200000` обязательно.** Клиентский дефолт — 30 секунд, а
  `statement_timeout` сервера — 2 минуты: без `timeoutMs` любые более-менее
  длительные запросы падают по клиентскому таймауту (именно этот симптом и
  приводил к перенастройке таймаутов). Три слоя, порядок строгий:
  `--statement-timeout` (2мин) < `--query-timeout` (3мин) < `timeoutMs`
  (200000мс). Бинарь откажется стартовать, если query-timeout ≤
  statement-timeout. Поднимать — только все три вместе, например
  `args: ["--database","wb_data_prod","--statement-timeout","5m","--query-timeout","6m"]`
  + `"timeoutMs": 400000`.
- Prod и test — **разные имена серверов**, чтобы selftest физически не мог
  попасть в прод.
- Пароля в этом JSON нет и не должно быть.
- Схема строгая: неизвестные ключи → весь сервер выкидывается из списка.

После правки — **полный перезапуск ZCode** → Settings → MCP: оба сервера
зелёные, `toolCount: 3`.

## 8. Проверка работы агента

В свежей сессии ZCode:

1. `pg_list_tables` → список таблиц базы.
2. `SELECT round(100.0*1/3)::float8 AS x` → должно вернуться **число**
   (`33.33…`), не строка. Это проверка, что подхвачены свежие описания
   инструментов — запущенная сессия держит старый бинарь и старые описания
   до рестарта ZCode.
3. `SELECT i FROM generate_series(1,600) AS t(i)` → `row_count: 500`,
   `truncated: true`, `notice` с советом про агрегаты — проверка канала
   сигнализации об обрезке.
4. Из план-режима: благодаря явному `destructiveHint:false` в wire-JSON вызов
   проходит предикат план-режима ZCode (`isMcp && !destructive`). Нюанс:
   текущий бандл ZCode может спрашивать аппрув для всех MCP-инструментов
   безусловно — детали в README, раздел «UX caveat — approval in plan mode».

## 9. Возможности и ограничения, которые агент знает из описаний

Всё это зашито в Description инструментов (поэтому агенту не нужно ничего
объяснять отдельно):

- Один SQL-оператор на вызов; значения — через `$1,$2` плейсхолдеры; `;`
  внутри строковых литералов/доллар-кавычек — это данные, не разделители.
- `SHOW` и `EXPLAIN` разрешены; сессионные (`SET`, `BEGIN`, `PREPARE`, …) —
  отклоняются пречеком.
- Капы вывода: **500 строк / 48KB**, дальше `truncated: true` + `notice` с
  конкретным советом (агрегаты вместо выгрузок; для сверхширокой строки —
  `length()/left()/substring()`).
- `int8`/`numeric` приходят **JSON-строками** (точность; `nm_id` > 2^53).
  Для числа — каст в SQL: `count(*)::int`, `round(...)::float8`. ID во float
  не кастовать.
- Тяжёлая аналитика (мульти-минутные сканы, огромные GROUP BY) — **psql под
  той же ролью**, MCP для этого не предназначен:

```cmd
call "%USERPROFILE%\.config\mcp-pg-readonly\env.cmd"
set PGPASSWORD=%PG_RO_PWD%
set PGOPTIONS=-c statement_timeout=10min
"%ProgramFiles%\PostgreSQL\17\bin\psql.exe" -h 192.168.10.7 -p 15432 -U mcp_ro -d wb_data_prod ^
  -c "SELECT date_trunc('day', dt) AS day, count(*), sum(price) FROM sales GROUP BY 1"
```

  `PGOPTIONS` задаёт свой таймаут сессии (MCP-шные две минуты тут не
  действуют) — задавайте явно, не бегайте безлимитно. Роль SELECT-only, так
  что этот путь так же безопасен, как MCP. **Только `$PG_RO_PWD`/`mcp_ro`**,
  никогда `PG_ADMIN_PWD`.

- Пока один запрос выполняется, следующий вызов в той же сессии ждёт
  (`MaxConns=1` — вызовы инструмента серийны).
- Ошибки инструментов приходят внутри результата (`IsError=true`), а не
  протокольной ошибкой — агент может самокорректироваться; таймауты
  транслируются в человекочитаемые подсказки (SQLSTATE 57014 и т.п.).

## 10. Git и секреты — правила, которые нельзя нарушать

Это не теория: однажды «деплойный черновик» унёс оба пароля в **публичный**
репо. Force-push историю переписал, но orphan-коммит оставался доступен по
прямому URL на GitHub ~90 дней; единственное реальное закрытие дыры — ротация
пароля на сервере.

1. **Placeholders — всегда.** В любых доках/примерах/коммитах:
   `set PG_RO_PWD=<your_password>`, никогда реальное значение. «Это черновик,
   потом исправлю» — так оно и утекает.
2. **Секреты живут вне репо** по дизайну: env.cmd с ACL (раздел 4). Репо
   полностью полезен и без реальных значений.
3. **Pre-commit gate** (в Git Bash перед каждым коммитом):
   `git diff --cached | grep -iE 'password|pwd|passwd|secret|token'` —
   хиты вида `= <реальное значение>` → стоп и заменить.
4. **Если секрет попал в коммит** — до пуша: (а) ротация на сервере
   (`ALTER ROLE mcp_ro PASSWORD '...'`) — единственное, что реально закрывает
   дыру; (б) rewrite истории (`commit --amend` / `filter-repo`). После
   force-push проверять утечку по **URL**
   `github.com/ilkoid/mini-mcp/commit/<sha>`, а не по `git log` — orphan
   в историю не входит, но по прямой ссылке жив.
5. **Ротация = обновить все копии**: `~/.config/mcp-pg-readonly/env` на Mac,
   `env.cmd` на каждом Windows-хосте, копия `PG_RO_PWD` в `poncho-ai/.env`.
6. `.gitattributes` не трогать: `*.cmd`/`*.bat` — CRLF, остальное LF,
   `*.exe` — binary; catch-all `*` идёт первым (last-match-wins).

## 11. Обновление в будущем

```cmd
cd /d %USERPROFILE%\dev\mini-mcp
git pull
go build -o %USERPROFILE%\bin\mcp-pg-readonly.exe .
copy /Y mcp-pg-readonly-run.cmd %USERPROFILE%\bin\mcp-pg-readonly-run.cmd
```

Затем **полный рестарт ZCode** — запущенная сессия держит старый бинарь и
старые описания инструментов. Если `git pull` меняет `.cmd` и что-то
ругается на концы строк — не «чинить» CRLF в `.cmd`, это канонический формат
(cmd.exe к тому же толерантен к LF; концы строк почти никогда не корень
проблемы — смотреть сначала на env.cmd и его `set`-синтаксис).

## 12. Troubleshooting

| Симптом | Причина | Фикс |
|---|---|---|
| Settings → MCP: `toolCount=0` / сервер не стартует | посторонний байт в stdout (stdio-транспорт) | бинарь пишет логи только в stderr по дизайну; если правите код — не добавлять `fmt.Println`. Также проверить, что `command` указывает на launcher, а не на `.exe` без пароля |
| `error: empty password` | кавычки/спецсимвол в `set`-строке; env.cmd не там; ACL не тому юзеру | раздел 4; проверить: `cmd /c call env.cmd && echo [%PG_RO_PWD:~0,2%]` (первые 2 символа, не весь пароль) |
| Запрос падает ровно через ~30с | нет `timeoutMs` (клиентский дефолт) | раздел 7 |
| `57014 query_canceled` / «query exceeded statement_timeout» | запрос дольше 2мин | фильтры/агрегаты/лимиты, psql-bypass, или поднять все три таймаута |
| `connection refused` / timeout | сеть, pg_hba, firewall | раздел 1 |
| `too many connections for role "mcp_ro"` | >8 конкурентных сессий роли | `ALTER ROLE mcp_ro CONNECTION LIMIT 16;` — не MaxConns |
| `count(*)` приходит строкой `"3"` | by design (точность) | `::int` / `::float8` в SQL |
| Сервер исчез из списка после правки config.json | неизвестный ключ в JSON / `command` массивом | схема строгая; `command` — строка |

## Чеклист

- [ ] Сеть: `Test-NetConnection` до PG-хоста проходит; pg_hba/firewall на PG-хосте пускают IP VPS
- [ ] Git, Go ≥ 1.25, (опц. psql) установлены; новое окно терминала
- [ ] `git clone` → `go build` → wrapper скопирован в `%USERPROFILE%\bin`
- [ ] `go test ./...` зелёный
- [ ] env.cmd: `set` без кавычек, hex-пароль, icacls закрыт
- [ ] (свой PG) role-bootstrap.sql по test → прод, с реальным владельцем таблиц
- [ ] Selftest PASS на test, затем на прод
- [ ] config.json: `cli` (не v2), command-строка на `.cmd`, `timeoutMs: 200000`, отдельные имена prod/test
- [ ] Полный рестарт ZCode → оба сервера зелёные, `toolCount: 3`
- [ ] `SELECT round(100.0*1/3)::float8` → число; `generate_series(1,600)` → truncated-notice
- [ ] Прочитан раздел 10 (git и секреты)
