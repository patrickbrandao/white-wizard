# Backup e restauração do painel

O `wwz-panel` guarda usuários, sessões, auditoria e eventos num único SQLite (`WWZ_PANEL_DB`). O Valkey é descartável e não tem backup (ADR 0009). Contrato completo na spec 016, seção "Backup e restauração".

## Backup agendado

Com `WWZ_PANEL_BACKUP_DIR` (ou `backup.dir` no `panel.yaml`) definido, o `serve` grava um snapshot por dia no horário UTC de `WWZ_PANEL_BACKUP_AT` (padrão `03:00`):

- arquivo `<dir>/panel-AAAAMMDD.db` (data UTC); outro snapshot no mesmo dia substitui o anterior;
- gerado com `VACUUM INTO` num temporário `.panel-AAAAMMDD.db.tmp` e renomeado, então nunca há snapshot pela metade;
- ficam os `WWZ_PANEL_BACKUP_KEEP` mais recentes (padrão 7); os mais antigos são apagados pelo nome;
- o horário do último snapshot vai para `settings.last_backup_at` e para a métrica `wwz_panel_last_backup_timestamp_seconds` de `/metrics`; o log registra `painel: backup` com o caminho.

## Backup sob demanda

```sh
wwz-panel backup                 # usa WWZ_PANEL_BACKUP_DIR
wwz-panel backup --dir /backup   # diretório explícito
```

Pode rodar com o painel no ar (`VACUUM INTO` lê uma visão consistente). Imprime o caminho do snapshot. Sem `--dir` nem `WWZ_PANEL_BACKUP_DIR`, sai com código 2; banco inexistente ou falha, código 1.

## Restauração

1. Pare o `serve` (ele mantém um `flock` exclusivo em `WWZ_PANEL_DB.lock`; com ele no ar o restore sai com código 1 e "painel em execução").
2. Rode:

   ```sh
   wwz-panel restore /backup/panel-20261031.db
   ```

   O comando copia o snapshot para `WWZ_PANEL_DB.restore.tmp` (com `fsync`), confere `PRAGMA integrity_check` = `ok` e `user_version` entre 1 e a versão suportada, apaga o `-wal`/`-shm` do banco atual e renomeia a cópia sobre `WWZ_PANEL_DB`. Snapshot inválido não toca o banco atual.
3. Suba o `serve`. O operador inicial (`WWZ_PANEL_ADMIN_*`) não é recriado, porque a base restaurada já tem usuários.

No Docker, rode o restore num container parado com os mesmos volumes, por exemplo `docker compose stop wwz-panel` e depois `docker compose run --rm wwz-panel restore /backup/panel-20261031.db` (volumes `panel-data` em `/data` e `panel-backup` em `/backup`).
