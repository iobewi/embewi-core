# Politique de sécurité

Embewi pilote des devices physiques (MCU/ESP32) capables d'actionner du
hardware réel (moteurs, relais…). Une vulnérabilité ici a potentiellement un
impact physique, pas seulement logiciel — merci de nous laisser le temps de
corriger avant toute divulgation publique.

Ce document couvre les trois dépôts du projet :

- [`embewi-core`](https://github.com/iobewi/embewi-core) — contrôleur Kubernetes (ce dépôt)
- [`embewi`](https://github.com/iobewi/embewi) — contrat Core↔Agent (spec normative)
- [`embewi-agent-esp`](https://github.com/iobewi/embewi-agent-esp) — firmware device

## Signaler une vulnérabilité

**Ne pas ouvrir d'issue publique.** Utiliser la divulgation privée GitHub sur
le dépôt concerné : onglet **Security** → **Report a vulnerability**
(*Private vulnerability reporting*), disponible sur les trois dépôts.

Si ce mécanisme n'est pas accessible, contacter directement le mainteneur
(voir le profil GitHub [`@iOBEWi`](https://github.com/iOBEWi)).

Merci d'inclure :
- le dépôt et la version/commit concernés,
- une description du problème et son impact (accès inbound non authentifié,
  contournement OTA, fuite de token/clé, etc.),
- si possible, un scénario de reproduction.

## Délai de réponse

Accusé de réception sous 5 jours ouvrés. Le délai de correction dépend de la
sévérité — un problème permettant un accès inbound non authentifié ou un
contournement de la vérification de signature/digest OTA est traité en
priorité.

## Modèle de menace (résumé)

Détail complet : `contract/docs/embewi-contract-v2.md` (§1, [NORMATIF]) et
`docs/embewi-prod-security.md` (dépôt agent).

```text
Core verifies for efficiency.
Bootloader verifies for trust.
```

- **Racine de confiance** : le bootloader ESP32 (Secure Boot v2). Le Core
  vérifie signature + digest par efficacité, jamais par autorité — un ESP qui
  reçoit des octets sur `/ota/write` ne peut pas savoir cryptographiquement
  qu'ils viennent d'un Core légitime sans Secure Boot côté device.
- **Transport** : token Bearer par node sur HTTPS (MVP) ; mTLS visé en cible,
  pas encore MVP.
- **Rayon d'action d'un token compromis** : au-delà des endpoints inbound du
  device concerné, `heartbeat.ip` pilote l'`EndpointSlice` — un token
  compromis peut détourner le trafic applicatif du `Service` associé. Détecté
  (pas bloqué) côté Core via l'Event `HeartbeatIPMismatch`.
- **Provisioning (portail captif AP)** : le point le plus faible par
  nécessité — aucun secret partagé n'existe avant l'enrôlement. Résiduel
  accepté pour le MVP : MITM actif sur l'AP ouvert (pas le sniffing passif,
  bloqué par HTTPS sur le portail).
- **Prod vs dev** : Secure Boot v2, Flash Encryption, anti-rollback eFuse et
  vérification du certificat Core sortant sont **opt-in production**
  (`sdkconfig.defaults.prod` côté agent) — irréversibles une fois activés
  (opérations eFuse), donc jamais par défaut en dev.

## Versions supportées

Pas encore de ligne de versions stabilisée (pré-v1.0, protocole `v1alpha1`).
Une fois taggé, seule la dernière version mineure de chaque dépôt reçoit des
correctifs de sécurité.
