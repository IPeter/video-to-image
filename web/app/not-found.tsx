import Link from "next/link";

import { Button } from "@/components/ui/button";

export default function NotFound() {
  return (
    <section
      aria-labelledby="not-found-title"
      className="mx-auto flex max-w-lg flex-col items-center gap-4 px-4 py-20 text-center"
    >
      <p className="text-sm font-medium text-muted-foreground">404</p>
      <h1 id="not-found-title" className="font-heading text-2xl font-semibold tracking-tight">
        Page not found
      </h1>
      <p className="text-muted-foreground">The page you are looking for does not exist.</p>
      <Button asChild>
        <Link href="/">Upload a video</Link>
      </Button>
    </section>
  );
}
