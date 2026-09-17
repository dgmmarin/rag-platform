import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { Pagination } from "./Pagination";

describe("Pagination", () => {
  it("renders nothing when everything fits on one page", () => {
    const { container } = render(
      <Pagination page={0} pageCount={1} total={5} onPrev={vi.fn()} onNext={vi.fn()} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("shows the total and page indicator, and wires next", () => {
    const onNext = vi.fn();
    render(<Pagination page={0} pageCount={3} total={45} onPrev={vi.fn()} onNext={onNext} />);

    expect(screen.getByText("45 total")).toBeInTheDocument();
    expect(screen.getByText(/page 1 of 3/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /previous/i })).toBeDisabled();

    fireEvent.click(screen.getByRole("button", { name: /next/i }));
    expect(onNext).toHaveBeenCalled();
  });

  it("disables next on the last page and wires prev", () => {
    const onPrev = vi.fn();
    render(<Pagination page={2} pageCount={3} total={45} onPrev={onPrev} onNext={vi.fn()} />);

    expect(screen.getByRole("button", { name: /next/i })).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: /previous/i }));
    expect(onPrev).toHaveBeenCalled();
  });
});
