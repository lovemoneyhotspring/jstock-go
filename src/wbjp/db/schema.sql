-- wbjp の状態と監査証跡。
--
-- 設計方針:
--   「何を売買したか」より「なぜそう判断したか」を残すことを優先する。
--   自動売買のデバッグは、事後に判断を再構成できるかどうかで決まる。
--   すべての行が run_id で串刺しでき、1回の実行を丸ごと追える。
--
--   金額・数量は TEXT で持つ。SQLite の REAL は倍精度浮動小数なので、
--   株数や約定代金を入れると丸め誤差が入る。Python 側で Decimal に戻す。

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- 1回の実行サイクル。すべての記録の起点。
CREATE TABLE IF NOT EXISTS runs (
    run_id        TEXT PRIMARY KEY,
    started_at    TEXT NOT NULL,
    finished_at   TEXT,
    as_of         TEXT NOT NULL,          -- 判断の基準日
    env           TEXT NOT NULL,          -- uat / prod
    mode          TEXT NOT NULL,          -- live / dry_run / backtest
    equity        TEXT,
    cash          TEXT,
    status        TEXT NOT NULL DEFAULT 'running',
    error         TEXT
);

CREATE INDEX IF NOT EXISTS idx_runs_as_of ON runs(as_of);

-- 各戦略が出した個別の意見。合成前の生データ。
CREATE TABLE IF NOT EXISTS signals (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    strategy      TEXT NOT NULL,
    symbol        TEXT NOT NULL,
    direction     REAL NOT NULL,
    confidence    REAL NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    meta_json     TEXT
);

CREATE INDEX IF NOT EXISTS idx_signals_run ON signals(run_id);
CREATE INDEX IF NOT EXISTS idx_signals_symbol ON signals(symbol);

-- 合成後の結論。contributions_json に各戦略の寄与を残す。
CREATE TABLE IF NOT EXISTS combined_signals (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id             TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    symbol             TEXT NOT NULL,
    direction          REAL NOT NULL,
    contributions_json TEXT,
    reason             TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_combined_run ON combined_signals(run_id);

-- サイジングが決めた「あるべき建玉」。
CREATE TABLE IF NOT EXISTS targets (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id    TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    symbol    TEXT NOT NULL,
    quantity  TEXT NOT NULL,
    reason    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_targets_run ON targets(run_id);

-- 発注した注文。client_order_id が一意なので、
-- 同じ判断からの再発注はここで弾ける（冪等性の担保）。
CREATE TABLE IF NOT EXISTS orders (
    client_order_id  TEXT PRIMARY KEY,
    run_id           TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    broker_order_id  TEXT,
    symbol           TEXT NOT NULL,
    side             TEXT NOT NULL,
    order_type       TEXT NOT NULL,
    quantity         TEXT NOT NULL,
    limit_price      TEXT,
    status           TEXT NOT NULL,
    filled_quantity  TEXT NOT NULL DEFAULT '0',
    avg_fill_price   TEXT,
    reason           TEXT NOT NULL DEFAULT '',
    placed_at        TEXT NOT NULL,
    updated_at       TEXT
);

CREATE INDEX IF NOT EXISTS idx_orders_run ON orders(run_id);
CREATE INDEX IF NOT EXISTS idx_orders_symbol ON orders(symbol);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);

CREATE TABLE IF NOT EXISTS fills (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    client_order_id  TEXT NOT NULL,
    run_id           TEXT,
    symbol           TEXT NOT NULL,
    side             TEXT NOT NULL,
    quantity         TEXT NOT NULL,
    price            TEXT NOT NULL,
    fee              TEXT NOT NULL DEFAULT '0',
    filled_at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_fills_order ON fills(client_order_id);

-- 逆指値の代替。Webull JP が日本株の逆指値に対応していないため、
-- ストップ価格はここに持って自前で評価する。
CREATE TABLE IF NOT EXISTS stops (
    symbol        TEXT PRIMARY KEY,
    stop_price    TEXT NOT NULL,
    entry_price   TEXT NOT NULL,
    created_on    TEXT NOT NULL,
    trailing      INTEGER NOT NULL DEFAULT 0,
    atr_multiple  TEXT NOT NULL DEFAULT '2.0',
    highest_close TEXT,
    initial_stop_price TEXT,           -- 1R の基準。旧レコードは NULL（repo が移行する）
    initial_quantity   TEXT,           -- 2段階利確の基準となる設定時の建玉数
    scaled_out    INTEGER NOT NULL DEFAULT 0,  -- 1段目の利確が済んだか
    updated_at    TEXT
);

-- 拒否した注文とその理由。「なぜ発注されなかったか」を追うために要る。
CREATE TABLE IF NOT EXISTS risk_events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id    TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    symbol    TEXT NOT NULL,
    reason    TEXT NOT NULL,
    created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_risk_run ON risk_events(run_id);

-- 日次の建玉スナップショット。損益の推移を後から再構成できる。
CREATE TABLE IF NOT EXISTS position_snapshots (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
    as_of       TEXT NOT NULL,
    symbol      TEXT NOT NULL,
    quantity    TEXT NOT NULL,
    cost_price  TEXT NOT NULL,
    last_price  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_snapshots_as_of ON position_snapshots(as_of);
