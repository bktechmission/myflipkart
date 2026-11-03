#!/usr/bin/env bash
# Ask Sauby · MyFlipKart.com — creates .env with fresh random secrets.
# Developed by Bhupender Kumar Panwar · Product Manager: Saurabh Dahake
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ -f .env ]]; then echo ".env already exists (delete it to regenerate)"; exit 0; fi
rand() { openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }
sed -e "s/^SESSION_SECRET=.*/SESSION_SECRET=$(rand)/" \
    -e "s/^MOCK_IDP_CLIENT_SECRET=.*/MOCK_IDP_CLIENT_SECRET=$(rand)/" .env.example > .env
chmod 600 .env
echo "Created .env with random secrets. Edit it to add Google sign-in or an LLM."
