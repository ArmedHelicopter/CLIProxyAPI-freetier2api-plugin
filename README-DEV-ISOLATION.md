# Development isolation

Build and test this plugin only from the migrated workspace and only against:

`E:\_ryanDev\_kernel_stage_8.0.43`

Do not use the production installation at `D:\Program Files\EasyCLIProxyAPI-v0.3.43-Windows-amd64` for development, testing, smoke checks, or manual API requests.

For unattended runs:

- use isolated config/auth/log/state directories;
- use a dedicated non-production port;
- do not invoke privileged discovery/model-fetch helpers;
- do not start `discovery.live.exe` or any helper that requests elevation;
- use the local embedded model catalog when remote model refresh is not required;
- fail fast on missing permissions instead of waiting for a UAC prompt.

The production environment can only be changed by an explicitly approved release procedure after isolated tests pass.
