export function displayName(user?: { profile?: { name: string } }): string {
  return user.profile.name.trim();
}
