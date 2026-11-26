CREATE TABLE platform_outbox (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id     uuid NOT NULL UNIQUE,
    type         text NOT NULL,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

CREATE INDEX platform_outbox_unpublished_idx ON platform_outbox (id) WHERE published_at IS NULL;
CREATE INDEX platform_outbox_published_idx ON platform_outbox (published_at) WHERE published_at IS NOT NULL;
