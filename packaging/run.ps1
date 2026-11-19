param(
    [int]$Port = 8080,
    [string]$Name = "local-share"
)

$ErrorActionPreference = "Stop"
$Image = "__IMAGE__"

docker pull $Image
if ($LASTEXITCODE -ne 0) {
    throw "docker pull failed for $Image; the existing container was left untouched."
}

$Existing = docker ps -a --filter "name=^/$Name$" --format "{{.Names}}"
if ($Existing -eq $Name) {
    docker rm -f $Name | Out-Null
}

# Uploads are untrusted data: read-only root filesystem, uploads only in a
# noexec in-memory tmpfs, no capabilities, no privilege escalation.
docker run -d --name $Name -p "$($Port):8080" --restart unless-stopped `
    --user 65532:65532 `
    --read-only `
    --tmpfs "/tmp:rw,noexec,nosuid,nodev,size=2200m,mode=1777" `
    --cap-drop ALL `
    --security-opt no-new-privileges:true `
    --pids-limit 256 `
    $Image
if ($LASTEXITCODE -ne 0) {
    throw "docker run failed for $Image."
}
Write-Host "Local Share is running at http://localhost:$Port"
