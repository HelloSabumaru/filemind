# FileMind

FileMind is a self hosted file sharing app. Upload files, share a link, and let recipients download individual files or a ZIP. Transfers support passwords, expiry, download limits and resumable uploads.

Administrators manage users and storage limits. There is no public registration, and each user manages their own transfers.

## Run with Docker Compose

You need Docker Compose and a reverse proxy that provides HTTPS. From the repository directory:

```sh
cp .env.example .env
```

Edit `.env` with three different HTTPS origins, then route them to these local ports:

| Interface | Environment variable | Proxy upstream |
| --- | --- | --- |
| Uploads and personal transfers | `FILEMIND_OWNER_URL` | `127.0.0.1:9080` |
| Shared downloads | `FILEMIND_PUBLIC_URL` | `127.0.0.1:9081` |
| Administration | `FILEMIND_ADMIN_URL` | `127.0.0.1:9082` |

Separate hostnames are recommended: cookies are shared across ports on the same hostname. Restrict access to the admin origin if administration should stay private.

Start the app:

```sh
docker compose up -d --build
```

Open your admin origin at `/login`. On first run, choose an administrator username and password. This form is available only on the admin origin and closes permanently after account creation. There is no password file or default administrator password.

Create other accounts under **Admin → Users** and adjust defaults and limits under **Admin → Server settings**. Environment values seed these defaults; use Server settings for later changes.

Admin sign-in is restricted to the admin origin by default. To also allow it on the upload origin, set `FILEMIND_REQUIRE_PRIVATE_ADMIN_SIGN_IN=false` in `.env` and recreate the container. Uploads, Transfers, Settings and Admin then share the same navigation there.

## Local development

Requires Go **1.26.8 or newer** and Node **24**.

```sh
npm ci
npm run dev
```

- First run: open [localhost:9082/login](http://localhost:9082/login) and create your administrator account, then add users under **Admin → Users**.
- Uploads: [localhost:9080/login](http://localhost:9080/login). The local launcher also allows admin sign-in here after setup.
- Shared downloads use port 9081.

All listeners bind to localhost. Data and account changes persist in `.local/data-v2`; existing accounts keep their credentials and setup stays closed. Stop with **Ctrl+C**. The launcher uses development settings; use the Docker setup for deployment.

## Tests

After `npm ci`:

```sh
npx playwright install chromium
npm run check
npm run test:web
go test -race ./...
go vet ./...
```

## Gotchas

- **Reverse proxies:** preserve the browser's `Origin` header, allow 8 MiB upload chunks and disable buffering for download responses. Set `FILEMIND_TRUSTED_PROXY_CIDRS` only for proxies you trust; otherwise users behind a proxy share its IP limits.
- **Passwords and links:** production account passwords require at least 15 characters. Files are readable by the server, and share URLs grant access to transfers; treat those URLs as secrets.
- **Resuming:** use **Transfers → Resume upload** and reselect the original unfinished files. Expired files and files that reach their download limit are deleted automatically.
- **Deleting:** Delete permanently removes a transfer and its files. Failed file removals are retried automatically.
- **Busy or slow downloads:** concurrency limits can return HTTP 429; retry later. Stalled downloads time out, and long downloads below roughly 16 KiB/s can be interrupted.
- **Backups:** Docker stores data in the `filemind_data` volume at `/data`. Stop FileMind before copying the entire data directory, and back up `.env` separately. Run one instance per data directory. Historical database schemas are not migrated.
