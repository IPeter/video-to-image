import Link from "next/link";

/** Minimal header: the domain name, linking back to the upload page. */
export function SiteHeader({ name }: { name: string | null }) {
  return (
    <header className="border-b bg-background">
      <div className="mx-auto flex h-14 w-full max-w-5xl items-center px-4 sm:px-6">
        {name ? (
          <Link
            href="/"
            className="rounded-sm font-heading text-base font-semibold tracking-tight text-foreground outline-none focus-visible:ring-3 focus-visible:ring-ring/50"
          >
            {name}
          </Link>
        ) : (
          <span className="font-heading text-base font-semibold tracking-tight">Video to images</span>
        )}
      </div>
    </header>
  );
}
