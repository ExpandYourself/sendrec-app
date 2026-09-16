/**
 * Pulls the invite token out of a `?redirect=/invites/accept?token=...` value.
 * Login and Register both need it: an invite is the one thing that gets a new
 * account created on an instance with public registration turned off.
 */
export function inviteTokenFromRedirect(redirect: string | null): string | null {
  return new URLSearchParams(redirect?.split("?")[1] ?? "").get("token");
}
