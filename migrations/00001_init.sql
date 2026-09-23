-- +goose Up
-- Schema from plan/06-data-model.md. River's own tables are created by rivermigrate
-- before goose runs (see internal/store/migrate.go).

CREATE TYPE job_status AS ENUM ('queued', 'processing', 'completed', 'rejected', 'failed', 'expired');
CREATE TYPE step_status AS ENUM ('pending', 'running', 'done', 'failed', 'skipped');

CREATE TABLE jobs (
    id                 text        PRIMARY KEY,               -- job_ + ULID
    domain_key         text        NOT NULL,
    status             job_status  NOT NULL DEFAULT 'queued',
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz NOT NULL,                  -- created_at + domain.retentionHours
    client_ip_hash     text        NOT NULL DEFAULT '',
    video_path         text,                                  -- storage key of the original; NULL once deleted
    video_info         jsonb       NOT NULL,                  -- media.VideoInfo
    model_name         text        NOT NULL DEFAULT '',
    gate               jsonb,                                 -- { passed, confidence, reason, detectedSubject }
    video_summary      text        NOT NULL DEFAULT '',
    result_explanation text        NOT NULL DEFAULT '',
    missing_shots      jsonb       NOT NULL DEFAULT '[]',     -- string[] of shot IDs
    zip_path           text,
    error_code         text,                                  -- user-safe
    error_message      text,                                  -- user-safe
    internal_log       text        NOT NULL DEFAULT '',       -- stderr tails / raw model output; never returned to clients
    cost_usd           numeric(10, 6) NOT NULL DEFAULT 0      -- sum of usage_events.cost_usd
);

CREATE INDEX jobs_domain_key_idx ON jobs (domain_key);
CREATE INDEX jobs_expires_at_idx ON jobs (expires_at) WHERE status <> 'expired';
CREATE INDEX jobs_created_at_idx ON jobs (created_at);

CREATE TABLE job_steps (
    id          bigserial   PRIMARY KEY,
    job_id      text        NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    name        text        NOT NULL,                         -- preview, analyze, extract, score, select, export
    status      step_status NOT NULL DEFAULT 'pending',
    started_at  timestamptz,
    finished_at timestamptz,
    duration_ms integer,
    meta        jsonb       NOT NULL DEFAULT '{}',
    UNIQUE (job_id, name)
);

-- Model output after post-processing. Kept for evaluation (decision 11).
CREATE TABLE scenes (
    id        bigserial PRIMARY KEY,
    job_id    text      NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    idx       integer   NOT NULL,                             -- analyze.Scene.Idx
    start_sec real      NOT NULL,
    end_sec   real      NOT NULL,
    shot_id   text      NOT NULL,
    relevance real      NOT NULL,
    reason    text      NOT NULL DEFAULT '',
    raw_start text      NOT NULL DEFAULT '',                  -- what the model actually said
    raw_end   text      NOT NULL DEFAULT '',
    UNIQUE (job_id, idx)
);

-- Files are deleted with the scratch dir; rows are kept indefinitely (decision 11).
CREATE TABLE candidate_frames (
    id               text    PRIMARY KEY,                     -- cf_ + ULID
    job_id           text    NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    scene_id         bigint  REFERENCES scenes (id) ON DELETE CASCADE,
    shot_id          text    NOT NULL,
    timestamp_sec    real    NOT NULL,
    sharpness        real    NOT NULL DEFAULT 0,
    sharpness_z      real    NOT NULL DEFAULT 0,
    exposure_penalty real    NOT NULL DEFAULT 0,
    dhash            bigint  NOT NULL DEFAULT 0,              -- uint64 stored bit-for-bit as int64
    duplicate_of     text    REFERENCES candidate_frames (id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED,
    rejected_reason  text    NOT NULL DEFAULT '',             -- '', blurry, exposure, duplicate
    score            real    NOT NULL DEFAULT 0,
    selected         boolean NOT NULL DEFAULT false
);

CREATE INDEX candidate_frames_job_id_idx ON candidate_frames (job_id);

CREATE TABLE selected_images (
    id            text    PRIMARY KEY,                        -- img_ + ULID
    job_id        text    NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
    candidate_id  text    REFERENCES candidate_frames (id) ON DELETE SET NULL,
    idx           integer NOT NULL,                           -- 1-based display order
    shot_id       text    NOT NULL,
    timestamp_sec real    NOT NULL,
    width         integer NOT NULL,
    height        integer NOT NULL,
    reason        text    NOT NULL DEFAULT '',
    file_name     text    NOT NULL DEFAULT '',                -- download name, see export.FileName
    image_path    text,                                       -- NULL once expired
    thumb_path    text,
    bytes         integer NOT NULL DEFAULT 0,
    UNIQUE (job_id, idx)
);

-- One row per model call. Source of truth for cost reporting.
CREATE TABLE usage_events (
    id              bigserial   PRIMARY KEY,
    job_id          text        REFERENCES jobs (id) ON DELETE CASCADE,
    domain_key      text        NOT NULL,
    step            text        NOT NULL,                     -- analyze, rerank
    model           text        NOT NULL,
    input_tokens    integer     NOT NULL DEFAULT 0,
    output_tokens   integer     NOT NULL DEFAULT 0,
    thinking_tokens integer     NOT NULL DEFAULT 0,
    video_seconds   real        NOT NULL DEFAULT 0,
    cost_usd        numeric(10, 6) NOT NULL DEFAULT 0,
    latency_ms      integer     NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX usage_events_created_at_idx ON usage_events (created_at);
CREATE INDEX usage_events_job_id_idx ON usage_events (job_id);

-- +goose Down
DROP TABLE usage_events;
DROP TABLE selected_images;
DROP TABLE candidate_frames;
DROP TABLE scenes;
DROP TABLE job_steps;
DROP TABLE jobs;
DROP TYPE step_status;
DROP TYPE job_status;
