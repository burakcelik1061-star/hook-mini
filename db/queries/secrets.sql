-- Encryption of the endpoint signing secrets at rest (see internal/secretbox).

-- name: ListEndpointSecretsToReencrypt :many
-- Endpoints with a secret not (yet) sealed with the current key: plaintext
-- left from before encryption, or sealed with an older key. Keyset paginated
-- on id.
SELECT id, secret, previous_secret
FROM endpoints
WHERE id > sqlc.arg(after_id)::uuid
  AND (secret NOT LIKE sqlc.arg(current_prefix)::text || '%'
       OR previous_secret NOT LIKE sqlc.arg(current_prefix)::text || '%')
ORDER BY id
LIMIT sqlc.arg(batch_size);

-- name: ReplaceEndpointSecrets :execrows
-- Writes re-encrypted secrets, but only if the row still holds the values
-- they were made from. A secret rotated in the meantime is left alone (0
-- rows): the rotation already wrote a value sealed with the current key.
UPDATE endpoints
SET secret          = sqlc.arg(new_secret)::text,
    previous_secret = sqlc.narg(new_previous_secret)::text
WHERE id = sqlc.arg(id)
  AND secret = sqlc.arg(old_secret)::text
  AND previous_secret IS NOT DISTINCT FROM sqlc.narg(old_previous_secret)::text;

-- name: CountEndpointSecretsByKey :many
-- How many stored secrets each key sealed; key_id is '' for a value that is
-- not sealed at all (plaintext). Used at startup to refuse to run with
-- plaintext secrets in the database or without a key that sealed some.
SELECT key_id::text AS key_id, count(*)::bigint AS secrets
FROM (
    SELECT CASE WHEN secret LIKE 'enc:v1:%' THEN split_part(secret, ':', 3) ELSE '' END AS key_id
    FROM endpoints
    UNION ALL
    SELECT CASE WHEN previous_secret LIKE 'enc:v1:%' THEN split_part(previous_secret, ':', 3) ELSE '' END
    FROM endpoints
    WHERE previous_secret IS NOT NULL
) s
GROUP BY key_id
ORDER BY key_id;
