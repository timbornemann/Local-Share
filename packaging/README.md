# Local Share Docker Package

This package starts Local Share from the release Docker image:

```text
__IMAGE__
```

## Windows

```powershell
.\run.ps1
```

## macOS / Linux

```sh
chmod +x ./run.sh
./run.sh
```

## Docker Compose

```sh
docker compose up -d
```

Open [http://localhost:8080](http://localhost:8080).

Set a different host port with:

```powershell
.\run.ps1 -Port 9090
```

or:

```sh
PORT=9090 ./run.sh
```

The app stores shares only temporarily inside the container. Restarting or recreating the container clears all current shares.
