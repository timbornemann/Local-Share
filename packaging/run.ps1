param(
    [int]$Port = 8080,
    [string]$Name = "local-share"
)

$ErrorActionPreference = "Stop"
$Image = "__IMAGE__"

docker pull $Image

$Existing = docker ps -a --filter "name=^/$Name$" --format "{{.Names}}"
if ($Existing -eq $Name) {
    docker rm -f $Name | Out-Null
}

docker run -d --name $Name -p "$($Port):8080" --restart unless-stopped $Image
Write-Host "Local Share is running at http://localhost:$Port"
