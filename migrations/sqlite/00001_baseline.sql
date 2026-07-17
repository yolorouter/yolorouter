-- migrations/sqlite/00001_baseline.sql
-- M0 阶段没有业务表，这是一条无操作的占位迁移，仅用于满足
-- migrations/embed.go 里 //go:embed sqlite/*.sql 通配符必须至少匹配
-- 一个文件的编译要求。M1 起在这条之后追加真实业务表迁移。

-- +goose Up

-- +goose Down
