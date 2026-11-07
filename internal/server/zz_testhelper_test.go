package server

import "workbuddy2api/internal/auth"

func authForTest() auth.Auth {
	return auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
}
