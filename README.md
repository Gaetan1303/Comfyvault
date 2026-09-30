# comfyvault

Un petit service Go qui range tes workflows ComfyUI et tes prompts, et qui les
expose sous forme d'API devant ton ComfyUI.

- Les workflows (format API de ComfyUI) sont stockés avec un numéro de version.
- Les paramètres utiles (prompt positif, seed, steps, taille, checkpoint, LoRA...)
  sont déclarés une fois par workflow (`bindings`) et détectés automatiquement à
  l'import.
- Les prompts sont découpés en blocs réutilisables (qualité, sujet, négatif de
  base...), assemblés par presets, avec variables `{{hair}}` et nettoyage des
  doublons et des poids.
- Un appel `POST /api/v1/workflows/<slug>/run` injecte les valeurs, envoie le
  job à ComfyUI, suit son exécution et rapatrie les images, avec les paramètres
  exacts (seed comprise) pour pouvoir rejouer un résultat.

Aucune dépendance tierce : la bibliothèque standard de Go suffit, il n'y a donc
pas de `go mod download` à faire.

## Prérequis

- Go 1.22 ou plus récent
- un ComfyUI qui tourne et dont tu connais l'adresse (par défaut
  `http://127.0.0.1:8188`)
- `curl` pour les exemples ci-dessous

### Installer Go

Linux (Ubuntu 24.04 fournit déjà la 1.22) :

    sudo apt update && sudo apt install golang-go

Sur une distribution plus ancienne, prends l'archive officielle :

    curl -LO https://go.dev/dl/go1.22.12.linux-amd64.tar.gz
    sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.22.12.linux-amd64.tar.gz
    echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.profile && . ~/.profile

macOS :

    brew install go

Windows :

    winget install GoLang.Go

Vérification : `go version` doit afficher 1.22 ou plus.

### Préparer ComfyUI

1. Dans ComfyUI, ouvre les réglages et active les options développeur
   (« Enable Dev mode Options »). Le menu propose alors **Save (API Format)**.
2. Exporte tes workflows avec cette entrée. Le fichier « normal » (format UI,
   avec `nodes` et `links`) n'est pas exécutable par l'API et sera refusé à
   l'import avec un message explicite.
3. Si ComfyUI tourne sur une autre machine, lance-le avec `--listen` et mets
   son adresse dans `COMFY_URL`.

## Installation

    unzip comfyvault.zip && cd comfyvault
    cp .env.example .env        # adapte COMFY_URL si besoin
    make build                  # produit bin/comfyvault
    make test                   # optionnel

Sans `make` (Windows par exemple) :

    go build -o bin/comfyvault ./cmd/comfyvault
    go test ./...

## Configuration

Variables d'environnement, ou fichier `.env` à la racine (l'environnement réel
prend le dessus sur le fichier) :

| Variable | Défaut | Rôle |
|---|---|---|
| `COMFY_URL` | `http://127.0.0.1:8188` | adresse de ComfyUI |
| `COMFYVAULT_LISTEN` | `127.0.0.1:8080` | adresse d'écoute du service |
| `COMFYVAULT_DATA` | `./data` | dossier de stockage |
| `COMFYVAULT_API_KEY` | vide | si défini, exige `Authorization: Bearer <clé>` |
| `COMFYVAULT_POLL_INTERVAL` | `1s` | fréquence d'interrogation de ComfyUI |
| `COMFYVAULT_RUN_TIMEOUT` | `30m` | durée max d'un run avant abandon |

Le service n'a pas de TLS. Reste sur l'adresse locale, ou place-le derrière un
reverse proxy avant de l'ouvrir au réseau, et définis une clé d'API dans ce cas.

## Prise en main

### 1. Importer un workflow

    ./bin/comfyvault import -slug txt2img -name "Texte vers image" examples/workflow_api.example.json

La commande affiche les paramètres détectés, par exemple `positive` (obligatoire,
nœud 6), `negative`, `seed`, `steps`, `cfg`, `width`, `height`, `checkpoint`.
Réimporter le même fichier ne crée pas de nouvelle version ; un fichier ou des
bindings différents créent la version suivante. Pour voir les bindings avant
d'importer : `./bin/comfyvault suggest mon_workflow.json`.

### 2. Lancer le service

    ./bin/comfyvault serve

Vérification : `curl localhost:8080/healthz` indique aussi si ComfyUI répond.

### 3. Charger des blocs de prompt

    sh examples/seed.sh

Le script crée trois blocs (`quality-header`, `subject`, `negative-base`) et deux
presets (`portrait`, `negative-default`). Sous Windows, lance-le depuis Git Bash
ou WSL, ou reprends les commandes `curl` qu'il contient.

    curl -s -X POST localhost:8080/api/v1/prompts/render \
      -d '{"preset":"portrait","vars":{"hair":"silver","outfit":"kimono"}}'

### 4. Lancer une génération

    curl -s -X POST localhost:8080/api/v1/workflows/txt2img/run -d '{
      "positive": {"preset": "portrait", "vars": {"hair": "silver", "outfit": "kimono"}},
      "negative": {"preset": "negative-default"},
      "params":   {"steps": 30, "width": 832, "height": 1216, "seed": -1}
    }'

La réponse (202) contient l'identifiant du run. Un `seed` absent ou égal à `-1`
est tiré au hasard, et la valeur utilisée est enregistrée dans le run.

    curl -s localhost:8080/api/v1/runs/<id>
    curl -s -o image.png localhost:8080/api/v1/runs/<id>/outputs/<nom-du-fichier>

Statuts d'un run : `queued`, `running`, `succeeded`, `failed`. Si ComfyUI refuse
le workflow (modèle absent, valeur invalide), l'erreur de ComfyUI est renvoyée
telle quelle dans la réponse du `POST .../run`, avec le détail par nœud.

Avant de lancer, `GET /api/v1/workflows/<slug>/check` compare les nœuds du
workflow à ceux installés dans ton ComfyUI et liste ceux qui manquent.

## Bindings personnalisés

Un binding relie un nom de paramètre à une entrée littérale d'un nœud :

    [
      {"name": "positive", "node_id": "6", "input": "text",  "type": "string", "required": true},
      {"name": "steps",    "node_id": "3", "input": "steps", "type": "int",    "default": 28},
      {"name": "seed",     "node_id": "3", "input": "seed",  "type": "seed"}
    ]

Types : `string`, `int`, `float`, `bool`, `seed`. Plusieurs bindings peuvent
partager un nom (une même valeur alimente plusieurs nœuds, utile pour un modèle
à deux encodeurs de texte) à condition d'avoir le même type. Un binding ne peut
pas viser une entrée déjà reliée à un autre nœud. Enregistrement :

    ./bin/comfyvault import -slug txt2img -bindings bindings.json workflow_api.json

ou via `POST /api/v1/workflows` avec le champ `bindings`.

## Prompts

Un bloc est un fragment de texte, un preset est une liste ordonnée de blocs.
À l'assemblage, le texte est normalisé : espaces et retours à la ligne
compactés, doublons supprimés sans tenir compte de la casse (le premier gagne),
poids réécrits sous forme canonique (`(tag:1.0)` devient `tag`, `(tag:1.10)`
devient `(tag:1.1)`). Les virgules à l'intérieur de `()`, `[]` et `<>` ne
découpent pas (`<lora:nom:0.8>` reste un seul élément), et les parenthèses
échappées (`\(`) sont respectées.

Une variable `{{nom}}` sans valeur fournie fait échouer la requête avec la liste
complète des variables manquantes. Un bloc utilisé par un preset ne peut pas
être supprimé.

## API

| Méthode et chemin | Rôle |
|---|---|
| `GET /healthz` | état du service et de ComfyUI |
| `GET /api/v1/workflows` | liste (slug, version, paramètres) |
| `POST /api/v1/workflows` | publie une version : `slug`, `name`, `description`, `api`, `bindings` |
| `GET /api/v1/workflows/{slug}` | détail ; `?version=N`, `?api=1` pour inclure le JSON |
| `GET /api/v1/workflows/{slug}/check` | nœuds manquants dans ComfyUI |
| `POST /api/v1/workflows/{slug}/run` | lance un run : `version`, `params`, `positive`, `negative` |
| `GET, POST /api/v1/blocks` | lister, créer |
| `GET, PUT, DELETE /api/v1/blocks/{id}` | lire, modifier, supprimer |
| `GET, POST /api/v1/presets` | lister, créer |
| `GET, PUT, DELETE /api/v1/presets/{id}` | lire, modifier, supprimer |
| `POST /api/v1/prompts/render` | assemble un prompt sans lancer de run |
| `GET /api/v1/runs` | historique ; `?workflow=slug`, `?limit=N` |
| `GET /api/v1/runs/{id}` | statut, paramètres résolus, erreur éventuelle |
| `GET /api/v1/runs/{id}/outputs/{name}` | télécharge un fichier produit |

Les corps de requête sont lus strictement : un champ inconnu renvoie une 400
plutôt que d'être ignoré. Codes : 404 introuvable, 409 conflit, 422 requête
valide en JSON mais refusée (validation, ComfyUI), 502 ComfyUI injoignable.

## Stockage

Tout est en JSON lisible dans `COMFYVAULT_DATA`, écrit de façon atomique, donc
versionnable avec git si tu le souhaites :

    data/
      workflows/<slug>.json    toutes les versions d'un workflow et leurs bindings
      blocks.json
      presets.json
      runs/<id>.json           paramètres résolus, statut, fichiers produits
      outputs/<id>/            images téléchargées depuis ComfyUI

Au redémarrage, les runs restés en `queued` ou `running` sont repris. Seuls les
fichiers de type `output` sont rapatriés ; les aperçus temporaires (`PreviewImage`)
sont ignorés.

## Choix et limites

- Stockage en fichiers JSON plutôt qu'en SQLite : zéro dépendance, lisible et
  diffable, largement suffisant pour un usage solo. Une seule instance du
  service doit écrire dans un dossier de données. `comfyvault import` pendant
  que le service tourne fonctionne, mais si tu publies au même instant depuis
  l'API, le dernier écrit gagne ; préfère l'API quand le service est lancé.
- Le suivi d'un run interroge `/history` et `/queue` à intervalle fixe au lieu
  d'écouter le WebSocket de ComfyUI. Il n'y a donc pas de pourcentage
  d'avancement, seulement l'état (`queued`, `running`, terminé).
- Pas d'annulation de run pour l'instant.
- Le JSON du workflow est normalisé au stockage (clés triées) ; les champs de
  nœud autres que `class_type`, `inputs` et `_meta` ne sont pas conservés.
- Pas d'interface web : tout passe par l'API et la CLI.

## Développement

    make test     # go test ./...
    make vet      # go vet ./...
    make fmt      # gofmt

    cmd/comfyvault/    CLI et démarrage du service
    internal/config/   variables d'environnement et .env
    internal/workflow/ format API, bindings, injection, détection automatique
    internal/prompt/   blocs, variables, normalisation
    internal/store/    persistance JSON
    internal/comfy/    client HTTP de ComfyUI
    internal/runner/   soumission et suivi des jobs
    internal/api/      routes HTTP

Les tests d'intégration de `internal/api` simulent ComfyUI avec un serveur HTTP
local ; ils ne demandent aucun ComfyUI réel.
