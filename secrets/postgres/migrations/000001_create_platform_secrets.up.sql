CREATE TABLE platform_secrets (
    name        text PRIMARY KEY,
    format      smallint NOT NULL,
    key_id      text NOT NULL,
    wrapped_dek bytea NOT NULL,
    nonce       bytea NOT NULL,
    ciphertext  bytea NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX platform_secrets_key_id_idx ON platform_secrets (key_id);
