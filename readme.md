# Temperature Exporter pour Prometheus (hwmon)

Un petit exporter Prometheus, simple et robuste, qui expose les températures du système Linux à partir de /sys/class/hwmon. Conçu pour tourner sur Proxmox, Debian, Ubuntu et autres distributions, avec une surface d’attaque minimale.

- Binaire unique en Go, sans dépendances système
- Labels: chip, sensor, label
- Endpoints: /metrics, /healthz
- Packaging: Dockerfile distroless, unité systemd, Makefile

## Fonctionnement

Le service parcourt le répertoire /sys/class/hwmon, détecte les fichiers temp*_input (valeurs en millidegré Celsius) et expose des métriques en degrés Celsius via HTTP. Quand disponible, les fichiers temp*_label ou temp*_type sont utilisés comme libellés compréhensibles (Tctl, CPU, etc.).

Métriques principales:

- temp_exporter_temperature_celsius{chip="…", sensor="…", label="…"}
- temp_exporter_scrape_duration_seconds

## Installation

Choisissez l’une des deux méthodes ci-dessous.

### Option A — Binaire depuis GitHub Releases (recommandé)

1) Aller sur la page des releases et télécharger le binaire adapté à votre architecture:

	- https://github.com/Tutanka01/Temperature-Exporter-Proxmox/releases
	- Fichiers disponibles: `temperature-exporter-linux-amd64`, `temperature-exporter-linux-arm64` et `SHA256SUMS`.

2) Vérifier l’intégrité (fortement conseillé):

```bash
sha256sum -c SHA256SUMS 2>/dev/null | grep temperature-exporter-linux-$( [ "$(uname -m)" = "x86_64" ] && echo amd64 || echo arm64 )
```

3) Installer le binaire

```bash
sudo install -m 0755 temperature-exporter-linux-$( [ "$(uname -m)" = "x86_64" ] && echo amd64 || echo arm64 ) /usr/local/bin/temperature-exporter
```

4) Lancer pour tester

```bash
/usr/local/bin/temperature-exporter -listen=":9102"
```

### Option B — Build depuis les sources

Prérequis: Go 1.22+.

```bash
make build
./bin/temperature-exporter -listen=":9102"
```

Test rapide (quelle que soit l’option):

```bash
curl -sf http://127.0.0.1:9102/healthz
curl -sf http://127.0.0.1:9102/metrics | head
```

## Déploiement systemd (hôte Proxmox/Linux)

1) Copier le binaire

```bash
# Si vous avez pris le binaire depuis les Releases (Option A):
# sudo install -m 0755 temperature-exporter-linux-amd64 /usr/local/bin/temperature-exporter
# ou pour ARM64:
# sudo install -m 0755 temperature-exporter-linux-arm64 /usr/local/bin/temperature-exporter

# Si vous avez build depuis les sources (Option B):
sudo install -m 0755 bin/temperature-exporter /usr/local/bin/temperature-exporter
```

2) Installer le service

```bash
sudo install -m 0644 packaging/temperature-exporter.service /etc/systemd/system/temperature-exporter.service
sudo systemctl daemon-reload
sudo systemctl enable --now temperature-exporter
```

3) Vérifier

```bash
systemctl status temperature-exporter
curl -sf http://127.0.0.1:9102/metrics | head
```

Note sécurité: l’unité est durcie (NoNewPrivileges, ProtectSystem, etc.) et octroie CAP_DAC_READ_SEARCH uniquement pour lire /sys. Si votre /sys est monté différemment, adaptez ReadOnlyPaths.

## Déploiement via Docker

Construire l’image:

```bash
docker build -t temp-exporter:latest .
```

Lancer (lecture de /sys en read-only):

```bash
docker run --rm -p 9102:9102 \
	--read-only \
	-v /sys/class/hwmon:/sys/class/hwmon:ro \
	--cap-drop ALL \
	temp-exporter:latest -listen=":9102" -hwmon="/sys/class/hwmon"
```

Astuce Podman/Rootless: montez /sys/class/hwmon en lecture seule et conservez cap-drop ALL (le binaire n’a pas besoin de capacités root en conteneur).

## Configuration Prometheus

Ajoutez un job de scrape dans prometheus.yml:

```yaml
scrape_configs:
	- job_name: 'temperature_exporter'
		static_configs:
			- targets: ['HOST_IP:9102']
```

Métriques exposées sur /metrics.

## Options CLI

- -listen string: adresse d’écoute (par défaut ":9102")
- -path string: chemin HTTP des métriques (par défaut "/metrics")
- -hwmon string: base des capteurs (par défaut "/sys/class/hwmon")
- -namespace string: préfixe des métriques (par défaut "temp_exporter")
- timeouts HTTP réglables: -read-timeout, -write-timeout, -read-header-timeout, -idle-timeout

## Sécurité et robustesse

- Binaire non-root recommandé; en systemd, capacité minimale CAP_DAC_READ_SEARCH pour lire /sys
- Pas d’entrée utilisateur; lecture en lecture seule de fichiers système
- Tolérance aux erreurs: capteurs manquants/illisibles ignorés proprement
- Serveur HTTP avec timeouts et arrêt gracieux sur SIGTERM

## Dépannage

- Aucune métrique temp_exporter_temperature_celsius n’apparaît:
	- Vérifiez la présence de /sys/class/hwmon
	- Vérifiez les permissions: sur un hôte strict, donnez CAP_DAC_READ_SEARCH au service
	- Certains environnements virtuels n’exposent pas les capteurs; installez lm-sensors et chargez les modules nécessaires

- Erreurs de build:
	- Vérifiez votre Go >= 1.22; sinon, utilisez le Dockerfile fourni

## Licence

MIT. Voir l’en-tête du dépôt si nécessaire.
