-- name: CreateUser :one
INSERT INTO users (email, password_hash, plan)
VALUES (@email, @password_hash, @plan)
RETURNING *;

-- name: GetUserByEmail :one
SELECT *
FROM users
WHERE email = @email;

-- name: GetUserByID :one
SELECT *
FROM users
WHERE id = @id;
