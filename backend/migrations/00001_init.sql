-- +goose Up
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE users (
    id            bigserial PRIMARY KEY,
    email         text NOT NULL,
    password_hash text NOT NULL,
    full_name     text NOT NULL,
    role          text NOT NULL CHECK (role IN ('customer','box_office','hall_manager','programmer','accounting','gate')),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX users_email_uq ON users (lower(email));

CREATE TABLE refresh_tokens (
    id          bigserial PRIMARY KEY,
    user_id     bigint NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash  bytea NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    revoked_at  timestamptz
);

CREATE TABLE halls (
    id    bigserial PRIMARY KEY,
    name  text NOT NULL UNIQUE,
    rows  int  NOT NULL CHECK (rows BETWEEN 1 AND 40),
    cols  int  NOT NULL CHECK (cols BETWEEN 1 AND 60)
);

CREATE TABLE seats (
    id       bigserial PRIMARY KEY,
    hall_id  bigint NOT NULL REFERENCES halls(id) ON DELETE CASCADE,
    row_no   int    NOT NULL CHECK (row_no >= 1),
    col_no   int    NOT NULL CHECK (col_no >= 1),
    label    text   NOT NULL,
    kind     text   NOT NULL DEFAULT 'standard' CHECK (kind IN ('standard','vip','accessible','blocked')),
    UNIQUE (hall_id, row_no, col_no)
);

CREATE TABLE movies (
    id                bigserial PRIMARY KEY,
    title             text NOT NULL,
    kind              text NOT NULL DEFAULT 'film' CHECK (kind IN ('film','play')),
    duration_min      int  NOT NULL CHECK (duration_min BETWEEN 30 AND 300),
    distributor       text NOT NULL,
    release_date      date NOT NULL,
    min_weekly_shows  int  NOT NULL DEFAULT 0 CHECK (min_weekly_shows >= 0),
    min_prime_bp      int  NOT NULL DEFAULT 0 CHECK (min_prime_bp BETWEEN 0 AND 10000),
    share_week1_bp    int  NOT NULL CHECK (share_week1_bp BETWEEN 0 AND 10000),
    share_week2_bp    int  NOT NULL CHECK (share_week2_bp BETWEEN 0 AND 10000),
    share_later_bp    int  NOT NULL CHECK (share_later_bp BETWEEN 0 AND 10000)
);

-- busy_until = end of film + cleaning; the exclusion constraint makes
-- overlapping showtimes in one hall impossible at the database level.
CREATE TABLE showtimes (
    id                bigserial PRIMARY KEY,
    movie_id          bigint NOT NULL REFERENCES movies(id),
    hall_id           bigint NOT NULL REFERENCES halls(id),
    starts_at         timestamptz NOT NULL,
    ends_at           timestamptz NOT NULL,
    busy_until        timestamptz NOT NULL,
    base_price_kurus  bigint NOT NULL CHECK (base_price_kurus > 0),
    capacity          int    NOT NULL CHECK (capacity > 0),
    CHECK (ends_at > starts_at AND busy_until >= ends_at),
    EXCLUDE USING gist (hall_id WITH =, tstzrange(starts_at, busy_until) WITH &&)
);
CREATE INDEX showtimes_starts_idx ON showtimes (starts_at, id);
CREATE INDEX showtimes_movie_idx ON showtimes (movie_id, starts_at);

CREATE TABLE holds (
    id              uuid PRIMARY KEY,
    user_id         bigint NOT NULL REFERENCES users(id),
    showtime_id     bigint NOT NULL REFERENCES showtimes(id),
    status          text NOT NULL DEFAULT 'active' CHECK (status IN ('active','expired','released','converted')),
    expires_at      timestamptz NOT NULL,
    extensions      int NOT NULL DEFAULT 0 CHECK (extensions BETWEEN 0 AND 2),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX holds_active_exp_idx ON holds (expires_at) WHERE status = 'active';

-- One row per seat per showtime: the unit of concurrency control.
CREATE TABLE show_seats (
    showtime_id  bigint NOT NULL REFERENCES showtimes(id) ON DELETE CASCADE,
    seat_id      bigint NOT NULL REFERENCES seats(id),
    status       text   NOT NULL DEFAULT 'free' CHECK (status IN ('free','held','sold')),
    hold_id      uuid REFERENCES holds(id),
    held_until   timestamptz,
    PRIMARY KEY (showtime_id, seat_id),
    CHECK ((status = 'held') = (hold_id IS NOT NULL AND held_until IS NOT NULL))
);
CREATE INDEX show_seats_expiry_idx ON show_seats (held_until) WHERE status = 'held';
CREATE INDEX show_seats_hold_idx ON show_seats (hold_id) WHERE hold_id IS NOT NULL;

CREATE TABLE coupons (
    code        text PRIMARY KEY,
    percent     boolean NOT NULL,
    value       bigint  NOT NULL CHECK (value > 0),
    min_total   bigint  NOT NULL DEFAULT 0 CHECK (min_total >= 0),
    max_uses    int     NOT NULL CHECK (max_uses > 0),
    used_count  int     NOT NULL DEFAULT 0,
    valid_from  timestamptz NOT NULL,
    valid_to    timestamptz NOT NULL,
    CHECK (used_count BETWEEN 0 AND max_uses),
    CHECK (NOT percent OR value <= 10000)
);

CREATE TABLE gift_cards (
    code            text PRIMARY KEY,
    initial_kurus   bigint NOT NULL CHECK (initial_kurus > 0),
    balance_kurus   bigint NOT NULL CHECK (balance_kurus >= 0),
    created_at      timestamptz NOT NULL DEFAULT now(),
    CHECK (balance_kurus <= initial_kurus)
);

CREATE TABLE orders (
    id              uuid PRIMARY KEY,
    user_id         bigint NOT NULL REFERENCES users(id),
    showtime_id     bigint NOT NULL REFERENCES showtimes(id),
    channel         text NOT NULL CHECK (channel IN ('online','box_office')),
    status          text NOT NULL CHECK (status IN ('paid','partially_refunded','refunded')),
    subtotal_kurus  bigint NOT NULL CHECK (subtotal_kurus >= 0),
    discount_kurus  bigint NOT NULL CHECK (discount_kurus >= 0),
    total_kurus     bigint NOT NULL CHECK (total_kurus >= 0),
    gift_kurus      bigint NOT NULL DEFAULT 0 CHECK (gift_kurus >= 0),
    coupon_code     text REFERENCES coupons(code),
    gift_card_code  text REFERENCES gift_cards(code),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX orders_user_idx ON orders (user_id, created_at DESC, id);
CREATE INDEX orders_created_idx ON orders (created_at, id);

-- Order lines: tickets and combos. Per-line amounts are stored so refunds
-- reverse exactly what was charged.
CREATE TABLE order_lines (
    id              bigserial PRIMARY KEY,
    order_id        uuid NOT NULL REFERENCES orders(id),
    kind            text NOT NULL CHECK (kind IN ('ticket','combo')),
    ticket_id       uuid UNIQUE,
    showtime_id     bigint NOT NULL REFERENCES showtimes(id),
    seat_id         bigint REFERENCES seats(id),
    category        text CHECK (category IN ('full','student')),
    gross_kurus     bigint NOT NULL CHECK (gross_kurus >= 0),
    discount_kurus  bigint NOT NULL CHECK (discount_kurus >= 0),
    paid_kurus      bigint NOT NULL CHECK (paid_kurus >= 0),
    gift_kurus      bigint NOT NULL CHECK (gift_kurus >= 0),
    net_kurus       bigint NOT NULL,
    ent_tax_kurus   bigint NOT NULL,
    vat_kurus       bigint NOT NULL,
    status          text NOT NULL DEFAULT 'valid' CHECK (status IN ('valid','used','refunded')),
    used_at         timestamptz,
    used_gate       text,
    refunded_at     timestamptz,
    CHECK (paid_kurus = gross_kurus - discount_kurus),
    CHECK (net_kurus + ent_tax_kurus + vat_kurus = paid_kurus),
    CHECK ((kind = 'ticket') = (seat_id IS NOT NULL AND ticket_id IS NOT NULL))
);
-- The last line of defence against double selling: a seat can have at most
-- one non-refunded ticket per showtime.
CREATE UNIQUE INDEX order_lines_one_ticket_per_seat
    ON order_lines (showtime_id, seat_id) WHERE kind = 'ticket' AND status <> 'refunded';
CREATE INDEX order_lines_order_idx ON order_lines (order_id);

CREATE TABLE gift_card_tx (
    id          bigserial PRIMARY KEY,
    code        text NOT NULL REFERENCES gift_cards(code),
    order_id    uuid REFERENCES orders(id),
    amount      bigint NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE gate_scans (
    id          bigserial PRIMARY KEY,
    ticket_id   uuid NOT NULL,
    gate_id     text NOT NULL,
    scanned_at  timestamptz NOT NULL,
    offline     boolean NOT NULL DEFAULT false,
    result      text NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (ticket_id, gate_id, scanned_at)
);

CREATE TABLE gate_conflicts (
    id          bigserial PRIMARY KEY,
    kind        text NOT NULL,
    ticket_id   uuid NOT NULL,
    gate_id     text NOT NULL,
    scanned_at  timestamptz NOT NULL,
    first_gate  text,
    first_at    timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE idempotency_keys (
    user_id       bigint NOT NULL,
    key           text   NOT NULL,
    request_hash  bytea  NOT NULL,
    status_code   int,
    response      bytea,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);

-- Append-only audit trail.
CREATE TABLE audit_log (
    id          bigserial PRIMARY KEY,
    actor_id    bigint,
    action      text NOT NULL,
    entity      text NOT NULL,
    entity_id   text NOT NULL,
    old_value   jsonb,
    new_value   jsonb,
    at          timestamptz NOT NULL DEFAULT now()
);

-- +goose StatementBegin
CREATE FUNCTION audit_log_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_log is append-only';
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER audit_log_no_update BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION audit_log_immutable();

-- +goose Down
DROP TABLE IF EXISTS audit_log, idempotency_keys, gate_conflicts, gate_scans, gift_card_tx,
    order_lines, orders, gift_cards, coupons, show_seats, holds, showtimes, movies, seats,
    halls, refresh_tokens, users CASCADE;
DROP FUNCTION IF EXISTS audit_log_immutable();
