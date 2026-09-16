import { useEffect, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { apiFetch } from "../api/client";
import { AuthForm } from "../components/AuthForm";
import { inviteTokenFromRedirect } from "../utils/invite";

export function Register() {
  const navigate = useNavigate();
  const [searchParams] = useSearchParams();
  const [ready, setReady] = useState(false);

  const redirect = searchParams.get("redirect");
  // A workspace invite arrives as ?redirect=/invites/accept?token=... The token
  // lets the invited address register even when public registration is off; the
  // backend re-validates it against the invited email.
  const inviteToken = inviteTokenFromRedirect(redirect);

  useEffect(() => {
    fetch("/api/health")
      .then((res) => res.json())
      .then((data: { registrationEnabled?: boolean }) => {
        if (data.registrationEnabled === false && !inviteToken) {
          navigate("/login", { replace: true });
        } else {
          setReady(true);
        }
      })
      .catch(() => setReady(true));
  }, [navigate, inviteToken]);

  if (!ready) return null;

  const loginPath = redirect ? `/login?redirect=${encodeURIComponent(redirect)}` : "/login";

  async function handleRegister(data: {
    email: string;
    password: string;
    name: string;
  }) {
    const res = await apiFetch<{
      message: string;
      requiresEmailConfirmation?: boolean;
    }>("/api/auth/register", {
      method: "POST",
      body: JSON.stringify({
        email: data.email,
        password: data.password,
        name: data.name,
        ...(inviteToken ? { inviteToken } : {}),
      }),
    });

    // Strict `=== false` is intentional: undefined (older server, malformed
    // response) routes to /check-email so the user is told to look for the
    // confirmation link, matching the pre-flag behaviour. Only an explicit
    // `false` short-circuits to /login. Preserve `?redirect=...` so invite
    // flows (AcceptInvite -> Register -> Login -> back to invite) keep working.
    if (res?.requiresEmailConfirmation === false) {
      navigate(loginPath, { state: { email: data.email, justRegistered: true } });
    } else {
      navigate("/check-email", { state: { email: data.email } });
    }
  }

  return (
    <AuthForm
      title="Create account"
      submitLabel="Create account"
      showName
      showPasswordConfirm
      onSubmit={handleRegister}
      footer={
        <>
          Already have an account? <Link to={loginPath}>Sign in</Link>
        </>
      }
    />
  );
}
