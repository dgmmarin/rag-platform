# Deployment Guide

This guide covers a single-region, per-tenant deployment.

## Prerequisites

- A Postgres 16 instance with the pgvector extension
- Object storage (S3 or MinIO)
- A KMS key for envelope encryption

## Provisioning a tenant

Run the control-plane command:

```bash
ragctl tenants create --name acme --region eu-west-1
```

## Settings

| Setting | Default | Notes |
| --- | --- | --- |
| target_tokens | 512 | chunk size |
| overlap_tokens | 64 | chunk overlap |

That is all you need to get started.
