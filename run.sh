#!/usr/bin/with-contenv bashio
set -e

# No flags. In add-on mode the binary reads /data/options.json for user
# options, takes the token from $SUPERVISOR_TOKEN, and hardcodes the rest:
# URL ws://supervisor/core/websocket, scripts at /addon_config/scripts,
# DB at /data/ha-lua.db.
exec /usr/local/bin/ha-lua
