<p align="center"><img src="docs/site/src/assets/logo.svg" alt="Le logo Ptah : une pierre de couronnement ambrée au-dessus de deux rangées bleu ciel sur un carré sombre aux coins arrondis" width="72" height="72"></p>

<h1 align="center">Ptah Operator</h1>

<p align="center">Un plan de contrôle Kubernetes qui fait converger les schémas PostgreSQL et MySQL à partir d’artefacts OCI immuables.</p>

<p align="center"><a href="README.md">English</a> · <a href="README.ja.md">日本語</a> · <a href="README.de.md">Deutsch</a> · <strong>Français</strong></p>

<p align="center">
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/ci.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/ci.yml?branch=master&label=ci&logo=github" alt="État du workflow CI sur master : vérification du code, détection des accès concurrents non synchronisés et cycle de vie Kubernetes complet"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/demo.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/demo.yml?branch=master&label=demonstration&logo=github" alt="État du workflow de démonstration hebdomadaire sur master, qui réenregistre chaque scénario sur un véritable cluster"></a>
  <a href="https://github.com/stokaro/ptah-operator/actions/workflows/docs.yml?query=branch%3Amaster"><img src="https://img.shields.io/github/actions/workflow/status/stokaro/ptah-operator/docs.yml?branch=master&label=docs&logo=github" alt="État du workflow de documentation sur master"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/LICENSE"><img src="https://img.shields.io/github/license/stokaro/ptah-operator?label=license&color=blue" alt="Licence : MIT"></a>
  <a href="https://github.com/stokaro/ptah-operator/blob/master/go.mod"><img src="https://img.shields.io/github/go-mod/go-version/stokaro/ptah-operator?label=go%20%E2%89%A5&logo=go&logoColor=white" alt="Version minimale de Go permettant de compiler ce module, déclarée dans go.mod"></a>
</p>

<p align="center"><a href="https://operator.ptah.run/edge/start/install/">Installation (en anglais)</a> · <a href="https://operator.ptah.run/edge/start/first-schema/">Premier schéma (en anglais)</a> · <a href="https://operator.ptah.run/demo/">Exécutions enregistrées (en anglais)</a> · <a href="https://operator.ptah.run/">Documentation (en anglais)</a> · <a href="https://docs.ptah.run/compatibility/operator/">Compatibilité Ptah (en anglais)</a></p>

Ptah Operator est un plan de contrôle natif de Kubernetes qui fait converger
en continu les schémas PostgreSQL et MySQL à partir d’artefacts OCI immuables.
Les opérations sur les bases s’exécutent dans des Jobs temporaires et durcis.
Le contrôleur lui-même n’a pas le droit de lire les Secrets des bases de données.

`PtahSchema` fait converger la structure de la base et les données de référence
nommées par une déclaration. `PtahMigration` exécute une séquence de migrations
préparée et vérifie que l’historique enregistré correspond à l’artefact choisi,
y compris lors d’une initialisation par checkpoint. Les deux fonctionnent avec
PostgreSQL et MySQL.

L’API est actuellement `v1alpha1`. La matrice de tests de bout en bout est verte :
toutes les versions mineures de Kubernetes prises en charge, les deux moteurs
et les deux formats d’artefacts. Il manque encore une version publiée pour
sortir du stade d’aperçu de l’implémentation. Les vérifications de chaque
exécution et le build Ptah utilisé sont consignés dans
[`support/ptah.json`](support/ptah.json).

## Modèle de réconciliation

```text
resolve tag to digest -> verify artifact -> observe database -> publish plan
          ^                                                          |
          |                 approval (when required) <---------------+
          |                                                          |
          +--- verify convergence <- apply exact approved plan <-----+
```

Le contrôleur enregistre une réservation avant de créer chaque Job. Il autorise
au plus un Job actif par `PtahSchema` et reprend cette réservation après un
redémarrage. La fin d’un processus ne prouve jamais la convergence : chaque
application est suivie d’une nouvelle observation en lecture seule.

Propriétés de sécurité :

- Les tags OCI sont résolus une fois. Tous les accès suivants utilisent le digest.
- Les plans lient les octets exacts à l’artefact, à la cible, à l’état observé,
  à la politique, à l’image du manager fixée par digest, à sa révision et à la
  sémantique d’état, à la version de Ptah, aux images de l’exécuteur et du runner,
  ainsi qu’au protocole du runner.
- Les approbations sont des ressources immuables distinctes, marquées avec
  l’identité authentifiée lors du contrôle d’admission.
- Les plans destructifs sont désactivés par défaut. Même après activation,
  ils exigent une approbation portant sur le plan exact.
- Les identifiants de la base et du registre restent isolés les uns des autres.
  Ils n’apparaissent jamais dans le statut, les Events, les ressources de plan
  ou les arguments de commande.
- La suppression et la suspension n’exécutent jamais de SQL de nettoyage.
- Un résultat d’application incertain entraîne une observation, sans réexécution.

## Lire le SQL appliqué

Le SQL réside dans des ConfigMaps immuables liées au plan par leur index,
leur taille et leur digest. Il n’est placé ni dans le statut ni dans les logs.
`kubectl ptah` le lit comme le fait l’opérateur :

```sh
kubectl ptah plan storefront --applied -n application -o sql
```

Ce client en lecture seule est publié avec chaque version sous forme de plugin
`kubectl` pour les plateformes clientes prises en charge.
[Lire un plan](https://operator.ptah.run/edge/use/read-a-plan/#install)
(en anglais) décrit son installation et les droits RBAC requis dans le namespace.

## Installation et premier schéma

Le guide se trouve sur [operator.ptah.run](https://operator.ptah.run/)
(en anglais). Il couvre l’installation, les trois valeurs que le chart exige
explicitement, un premier schéma complet, la configuration, l’exploitation,
le modèle de sécurité, les motifs des Conditions et les périodes de support.

```sh
cd docs/site && npm ci && npm run build
```

Cette commande construit la documentation à partir de cette copie du dépôt.

## Voir l’opérateur fonctionner

Les [exécutions enregistrées](https://operator.ptah.run/demo/) (en anglais)
sont des sessions de terminal réalisées sur un véritable cluster : appliquer
et modifier un schéma, approuver un plan, refuser une modification destructive,
corriger une dérive, rencontrer une erreur et reprendre l’exécution. Les
Conditions décrites sont vérifiées sur les objets réels pendant l’enregistrement.
La session n’est publiée que si toutes les vérifications réussissent.

Le badge `demonstration` indique l’état du réenregistrement hebdomadaire sur
`master`, pas celui des sessions affichées sur le site. S’il est vert, tous
les scénarios restent valides sur un cluster construit depuis `master`.
Le lecteur voit l’enregistrement conservé dans `demo/recordings/runs.json`,
qui ne change que lors d’une nouvelle capture. Un badge rouge signale donc
qu’une démonstration n’est plus reproductible.

[`demo/`](demo/README.md) explique en anglais comment la construire et l’exécuter.

## Documentation

Le guide en anglais est publié sur
[operator.ptah.run](https://operator.ptah.run/). Ses sources se trouvent dans
`docs/site/src/content/docs`. Une page s’adresse aux personnes qui modifient
le code :

- [Architecture](https://operator.ptah.run/edge/reference/architecture/)
  (en anglais) : les composants, leur emplacement et les invariants qu’ils maintiennent.

Ptah lui-même, ses formats de schéma, la structure des artefacts OCI et la CLI
sont documentés sur [docs.ptah.run](https://docs.ptah.run/edge/). La
[matrice de compatibilité](https://docs.ptah.run/compatibility/operator/)
indique les builds Ptah vérifiés avec cet opérateur. Ces ressources sont en anglais.

`PtahMigration` et `PtahSchema` restent distincts. Un ensemble de lignes déclaré
décrit les données souhaitées, tandis qu’une séquence de migrations décrit une
transition entre états. Les deux conservent donc leurs propres API et machines
à états, tout en partageant le transport OCI, l’isolation des identifiants,
le protocole d’exécution et la coordination des bases cibles.

## Licence et aide

Ptah Operator est publié sous [licence MIT](LICENSE).

Pour une question ou un rapport de bug, ouvrez une issue dans
[stokaro/ptah-operator](https://github.com/stokaro/ptah-operator/issues).
Le comportement de la CLI Ptah utilisée seule relève de
[stokaro/ptah](https://github.com/stokaro/ptah/issues).
[CONTRIBUTING.md](CONTRIBUTING.md) décrit les informations nécessaires à un
rapport exploitable et les vérifications requises pour une modification.
La participation est encadrée par le [code de conduite](CODE_OF_CONDUCT.md).
Ces ressources sont en anglais. Pour les demandes commerciales : `ask@stokaro.com`.
