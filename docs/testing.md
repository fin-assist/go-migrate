# Проверки go-migrate

FIN-251: публичные runner операции и `migratetest` документированы в [README](../README.md).

Unit (`TestUnit` / `FuzzUnit`) исполняется без БД и сети: private consumer interfaces изолируют queue connection и migration engine. Фейки проверяют ordering, argument/config propagation, errors.Is, cleanup и stop. Это не проверка корректности SQL.

Integration (`TestIntegration` / `FuzzIntegration`) использует настоящий PostgreSQL18: отдельные version tables, конкурентные очереди, timeouts, dirty/force/rollback и реальный up/down/up. API (`TestAPI` / `FuzzAPI`, исполняемые Example) — внешний package `migrate_test`: реальные публичные операции и harness с PG18 от лица deployment/test entrypoint. Никаких handler-only или переназванных fake API tests.

В test entrypoint внутренний pgtest выбирает TEST_DATABASE_URL или testcontainers; публичный migratetest получает adminDSN явно и не читает env. Недоступная БД при CI=true — ошибка; skip не считается успехом.

`make test` выполняет race inventory, tidy/vet, все Go suites с race/count1/timeout8m, Python regression guards и CHANGELOG check. Любой агентский запуск дополнительно ограничен process timeout. `make collect-unit`, `collect-integration`, `collect-api` собирают штатные profiles; `make check-unit`, `check-integration`, `check-api` независимо требуют80% statements закреплённым go-test-coverage/v2 v2.20.1. Принимается округление79,95%→80,0%. `make cover` выполняет три уровня и дополнительный informational combined через comma profiles.

Все уровни используют одинаковые packages из go list -race ./..., исключая **только точный** github.com/fin-assist/go-migrate/internal/pgtest. Suite всё ещё ./...: tests pgtest не потеряны. Public migratetest и все остальные обычные Go helpers включены. Никакого собственного калькулятора/реестра coverage. N/A guard сохраняет фиксированную identity исходного go-library-template; новая библиотека не наследует исключение. Empty/header-only/zero-statement profiles, empty/skip/fail suites отвергаются. Race inventory проверяет все Test/Fuzz имена; Example с Output исполняются как API.

Required CI job go собирает и проверяет каждый уровень даже при соседнем провале, сохраняет JSONL/profiles/native reports artifacts и падает при любом из шести неуспешных outcomes. Combined не компенсирует отдельный недостаток. lint/api/migrations, release и Renovate jobs сохраняются; controllers/protections не изменяются.

Локальный замер FIN-251 на Go1.26.0 / PostgreSQL18: Unit283/338 (83,7%), Integration294/338 (87,0%), API292/338 (86,4%); informational combined322/338 (95,3%). Все три знаменателя включают публичный migratetest. Актуальный current-head CI и review receipts фиксируются в PR. Mid-file cancellation прошла20 повторов с race; полный Unit audit с OS deny-network и unset TEST_DATABASE_URL прошёл.
