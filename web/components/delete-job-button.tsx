"use client";

import { Trash2 } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";

export interface DeleteJobButtonProps {
  onConfirm: () => void;
  deleting?: boolean;
}

/** "Delete now" with a confirmation dialog: deleting files cannot be undone. */
export function DeleteJobButton({ onConfirm, deleting = false }: DeleteJobButtonProps) {
  const [open, setOpen] = useState(false);
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button type="button" variant="ghost" disabled={deleting}>
          <Trash2 aria-hidden="true" />
          {deleting ? "Deleting…" : "Delete now"}
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete these images now?</DialogTitle>
          <DialogDescription>
            The images, the ZIP and your video are removed from our servers right away. This cannot be undone.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <DialogClose asChild>
            <Button type="button" variant="outline">
              Keep them
            </Button>
          </DialogClose>
          <Button
            type="button"
            variant="destructive"
            onClick={() => {
              setOpen(false);
              onConfirm();
            }}
          >
            Delete now
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
